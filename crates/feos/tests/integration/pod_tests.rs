// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use super::fixtures::{verify_microvm_cleanup, PodGuard};
use super::{ensure_server, get_public_clients, installed_guest, PUBLIC_SERVER_ADDRESS};
use anyhow::{Context, Result};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, ListContainersRequest,
};
use feos_proto::host_service::{
    host_service_client::HostServiceClient, GetNetworkConfigRequest, GetTimeRequest,
    GetVersionInfoRequest, PingRequest, PingResponse,
};
use feos_proto::pod_service::{
    pod_service_client::PodServiceClient, CreatePodRequest, DeletePodRequest, GetPodRequest,
    PodConsoleInput, PodInfo, PodNetwork, PodSpec, PodState, Resources, ResumePodRequest,
    StartPodRequest, StopPodRequest, SuspendPodRequest,
};
use feos_utils::vsock::API_PORT;
use log::info;
use microvm_service::vmm::jail;
use microvm_service::{vm_dir, vsock_path};
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use pod_service::network;
use std::path::Path;
use std::time::{Duration, SystemTime};
use tokio::time::timeout;
use tokio_stream::StreamExt;
use tonic::transport::Channel;

/// The user a process runs as.
fn process_uid(pid: i64) -> Result<u32> {
    let status = std::fs::read_to_string(format!("/proc/{pid}/status"))?;
    status
        .lines()
        .find_map(|line| line.strip_prefix("Uid:"))
        .and_then(|uids| uids.split_whitespace().next())
        .and_then(|uid| uid.parse().ok())
        .context("No Uid in /proc/<pid>/status")
}

/// Checks that the pod's Firecracker runs in the pod's cgroup, with the
/// pod's share of the CPU, and returns the most memory the cgroup has
/// accounted, in MiB.
fn check_cgroup(pod_id: &str, pid: i64, vcpus: u32) -> Result<u64> {
    let cgroup = jail::cgroup(pod_id);
    let read = |file: &str| -> Result<String> {
        Ok(std::fs::read_to_string(cgroup.join(file))?
            .trim()
            .to_string())
    };
    assert!(
        read("cgroup.procs")?
            .lines()
            .any(|line| line == pid.to_string()),
        "Firecracker (pid {pid}) is not in {}",
        cgroup.display()
    );
    assert_eq!(read("cpu.weight")?, jail::cpu_weight(vcpus).to_string());
    assert_eq!(read("memory.max")?, "max");
    Ok(read("memory.peak")?.parse::<u64>()? >> 20)
}

fn new_pod_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

async fn get_pod(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<PodInfo> {
    Ok(pods
        .get_pod(GetPodRequest {
            pod_id: pod_id.to_string(),
        })
        .await?
        .into_inner())
}

/// Waits until the pod is in `state`; a pod that fails on the way fails
/// the wait, unless failing is what it waits for.
async fn wait_for_pod(
    pods: &mut PodServiceClient<Channel>,
    pod_id: &str,
    state: PodState,
) -> Result<PodInfo> {
    timeout(Duration::from_secs(60), async {
        loop {
            let pod = get_pod(pods, pod_id).await?;
            if pod.state() == state {
                return Ok(pod);
            }
            if pod.state() == PodState::Failed {
                anyhow::bail!("pod {pod_id} failed: {}", pod.reason);
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    })
    .await
    .with_context(|| format!("pod {pod_id} did not become {}", state.as_str_name()))?
}

/// The guest's host service, behind the pod's vsock.
async fn guest_host(pod_id: &str) -> Result<HostServiceClient<Channel>> {
    Ok(HostServiceClient::new(
        feos_utils::vsock::channel(vsock_path(pod_id), API_PORT).await?,
    ))
}

async fn guest_ping(pod_id: &str) -> Result<PingResponse> {
    Ok(guest_host(pod_id)
        .await?
        .ping(PingRequest {})
        .await?
        .into_inner())
}

/// How far the guest's clock is off.
async fn guest_clock_skew(pod_id: &str) -> Result<Duration> {
    let guest_time: SystemTime = guest_host(pod_id)
        .await?
        .get_time(GetTimeRequest {})
        .await?
        .into_inner()
        .time
        .context("GetTime returned no time")?
        .try_into()?;
    Ok(SystemTime::now()
        .duration_since(guest_time)
        .unwrap_or_else(|e| e.duration()))
}

/// A request for the pod's guest, through feosd.
fn for_pod<T>(pod_id: &str, message: T) -> tonic::Request<T> {
    let mut request = tonic::Request::new(message);
    request
        .metadata_mut()
        .insert("feos-pod-id", pod_id.parse().expect("a pod ID is ASCII"));
    request
}

#[tokio::test]
async fn test_pod_lifecycle() -> Result<()> {
    let Some(installed) = installed_guest()? else {
        return Ok(());
    };
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;

    // feosd gives the pod its network: a TAP device on the pods' bridge,
    // and an address.
    let pod_id = new_pod_id();
    let tap_name = network::tap_name(&pod_id);
    let spec = PodSpec {
        resources: Some(Resources {
            vcpus: 2,
            memory_mib: 512,
        }),
        containers: vec![],
        network: None,
        labels: [("test".to_string(), "lifecycle".to_string())].into(),
    };
    let create = CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec.clone()),
    };
    pods.create_pod(create.clone()).await?;
    let mut guard = PodGuard::new(pod_id.clone());

    // A create delivered twice finds the pod; another spec does not.
    let again = pods.create_pod(create).await?.into_inner().pod.unwrap();
    assert_eq!(again.spec, Some(spec.clone()));
    let mut other = spec.clone();
    other.resources = Some(Resources {
        vcpus: 1,
        memory_mib: 512,
    });
    let status = pods
        .create_pod(CreatePodRequest {
            pod_id: pod_id.clone(),
            spec: Some(other),
        })
        .await
        .expect_err("a create with another spec");
    assert_eq!(status.code(), tonic::Code::AlreadyExists);

    let pod = wait_for_pod(&mut pods, &pod_id, PodState::Running).await?;
    let microvm = pod.microvm.context("a running pod has a microVM")?;
    assert!(microvm.guest_attached);
    assert!(!microvm.guest_version.is_empty());
    let pid = microvm.pid;
    guard.set_pid(pid as i32);
    info!("Pod {pod_id} runs, Firecracker pid {pid}");

    // The guest has no RTC and no NTP: its clock and its network are what
    // feosd pushed when it attached.
    let network = guest_host(&pod_id)
        .await?
        .get_network_config(GetNetworkConfigRequest {})
        .await?
        .into_inner();
    let address: std::net::Ipv4Addr = pod.ipv4_address.parse()?;
    assert_eq!(
        address.octets()[..2],
        [10, 200],
        "{address} is not the pods'"
    );
    assert!(network.configured);
    assert_eq!(network.interface, "eth0");
    assert_eq!(network.ipv4_cidr, format!("{address}/16"));
    assert_eq!(network.gateway, "10.200.0.1");
    assert_eq!(
        network.dns,
        vec!["8.8.8.8".to_string(), "1.1.1.1".to_string()]
    );
    assert_eq!(network.hostname, pod_id);
    // An isolated port of the pods' bridge.
    let port = Path::new("/sys/class/net").join(&tap_name);
    let bridge = std::fs::read_link(port.join("master"))?;
    assert!(
        bridge.ends_with(network::BRIDGE),
        "{tap_name} is on {}",
        bridge.display()
    );
    assert_eq!(
        std::fs::read_to_string(port.join("brport/isolated"))?.trim(),
        "1"
    );
    let skew = guest_clock_skew(&pod_id).await?;
    assert!(
        skew < Duration::from_secs(5),
        "The guest's clock is {skew:?} off"
    );
    let first_boot = guest_ping(&pod_id).await?.boot_id;

    // Firecracker runs jailed, as a user of the pod's own, in a cgroup of
    // the pod's own, and boots the installed guest with the installed
    // Firecracker.
    assert_ne!(process_uid(pid)?, 0, "Firecracker runs as root");
    let peak = check_cgroup(&pod_id, pid, 2)?;
    info!("The pod's cgroup accounted at most {peak} MiB");
    let manifest = jail::Manifest::load(&pod_id)?;
    assert!(
        manifest
            .links
            .iter()
            .any(|link| link.source == installed.guest.join("vmlinuz")),
        "The pod does not boot {}: {manifest:?}",
        installed.guest.display()
    );
    assert_eq!(
        manifest.vmm.map(|vmm| vmm.firecracker),
        Some(installed.firecracker.join("firecracker"))
    );

    // feosd forwards the pod's services to its guest, and only those.
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let listed = containers
        .list_containers(for_pod(&pod_id, ListContainersRequest {}))
        .await?
        .into_inner();
    assert!(listed.containers.is_empty(), "The guest has no containers");
    let mut host = HostServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let refused = host
        .get_version_info(for_pod(&pod_id, GetVersionInfoRequest {}))
        .await
        .expect_err("the guest's host service through feosd");
    assert_eq!(refused.code(), tonic::Code::Unimplemented);
    let unknown = containers
        .list_containers(for_pod(&new_pod_id(), ListContainersRequest {}))
        .await
        .expect_err("a pod that is not there");
    assert_eq!(unknown.code(), tonic::Code::NotFound);

    info!("Reading the console while the guest logs a call");
    let (console_tx, console_rx) = tokio::sync::mpsc::channel(1);
    console_tx
        .send(PodConsoleInput {
            pod_id: pod_id.clone(),
            input: vec![],
        })
        .await?;
    let mut console = pods
        .stream_pod_console(tokio_stream::wrappers::ReceiverStream::new(console_rx))
        .await?
        .into_inner();
    // The console shows output from the moment it is attached.
    tokio::time::sleep(Duration::from_millis(200)).await;
    guest_host(&pod_id)
        .await?
        .get_version_info(GetVersionInfoRequest {})
        .await?;
    let mut output = String::new();
    timeout(Duration::from_secs(10), async {
        while !output.contains("GetVersionInfo") {
            match console.next().await {
                Some(Ok(chunk)) => output.push_str(&String::from_utf8_lossy(&chunk.output)),
                other => panic!("The console ended: {other:?}"),
            }
        }
    })
    .await
    .expect("The guest's log line did not show on the console");
    drop(console_tx);

    info!("Stopping pod {pod_id}");
    pods.stop_pod(StopPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Stopped);
    assert!(pod.microvm.is_none(), "A stopped pod has no microVM");
    let refused = pods
        .resume_pod(ResumePodRequest {
            pod_id: pod_id.clone(),
        })
        .await
        .expect_err("a resume of a stopped pod");
    assert_eq!(refused.code(), tonic::Code::FailedPrecondition);

    info!("Starting pod {pod_id} again");
    pods.start_pod(StartPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let pod = wait_for_pod(&mut pods, &pod_id, PodState::Running).await?;
    guard.set_pid(pod.microvm.context("a running pod has a microVM")?.pid as i32);
    assert_ne!(
        guest_ping(&pod_id).await?.boot_id,
        first_boot,
        "A start is a new boot"
    );

    info!("Deleting pod {pod_id}");
    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let gone = pods
        .get_pod(GetPodRequest {
            pod_id: pod_id.clone(),
        })
        .await
        .expect_err("a deleted pod");
    assert_eq!(gone.code(), tonic::Code::NotFound);
    verify_microvm_cleanup(&pod_id);
    assert!(
        !Path::new("/sys/class/net").join(&tap_name).exists(),
        "The pod's TAP device is gone with the pod"
    );
    assert!(
        !jail::cgroup(&pod_id).exists(),
        "The pod's cgroup is gone with the pod"
    );
    assert!(
        installed.guest.join("vmlinuz").exists(),
        "Deleting the pod leaves the installed guest"
    );
    guard.disable_cleanup();
    Ok(())
}

#[tokio::test]
async fn test_pod_crash() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;

    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(PodSpec {
            resources: Some(Resources {
                vcpus: 1,
                memory_mib: 256,
            }),
            // A pod with no network at all.
            network: Some(PodNetwork {
                none: true,
                ..Default::default()
            }),
            ..Default::default()
        }),
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_for_pod(&mut pods, &pod_id, PodState::Running).await?;
    let pid = pod.microvm.context("a running pod has a microVM")?.pid;
    guard.set_pid(pid as i32);

    info!("Killing Firecracker, pid {pid}");
    kill(Pid::from_raw(pid as i32), Signal::SIGKILL).context("Failed to kill Firecracker")?;
    let pod = timeout(Duration::from_secs(30), async {
        loop {
            let pod = get_pod(&mut pods, &pod_id).await?;
            if pod.state() == PodState::Failed {
                return anyhow::Ok(pod);
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    })
    .await
    .expect("Timed out waiting for the pod to fail")?;
    assert!(
        pod.reason.contains("crashed"),
        "A pod whose microVM crashed failed: {}",
        pod.reason
    );
    let refused = pods
        .suspend_pod(SuspendPodRequest {
            pod_id: pod_id.clone(),
        })
        .await
        .expect_err("a suspend of a failed pod");
    assert_eq!(refused.code(), tonic::Code::FailedPrecondition);

    info!("Starting the failed pod again");
    pods.start_pod(StartPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let pod = wait_for_pod(&mut pods, &pod_id, PodState::Running).await?;
    guard.set_pid(pod.microvm.context("a running pod has a microVM")?.pid as i32);

    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    verify_microvm_cleanup(&pod_id);
    guard.disable_cleanup();
    Ok(())
}

#[tokio::test]
async fn test_pod_suspend_and_resume() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;

    // The microVM layer hands the pod's TAP device to the pod's user, on
    // every restore.
    let pod_id = new_pod_id();
    let tap_name = network::tap_name(&pod_id);
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(PodSpec {
            resources: Some(Resources {
                vcpus: 1,
                memory_mib: 256,
            }),
            ..Default::default()
        }),
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_for_pod(&mut pods, &pod_id, PodState::Running).await?;
    guard.set_pid(pod.microvm.context("a running pod has a microVM")?.pid as i32);
    let first = guest_ping(&pod_id).await?;

    for round in 0..2 {
        info!("Suspending pod {pod_id}, round {round}");
        let memory_bytes = pods
            .suspend_pod(SuspendPodRequest {
                pod_id: pod_id.clone(),
            })
            .await?
            .into_inner()
            .memory_bytes;
        info!(
            "Pod is suspended; its memory takes {} MiB",
            memory_bytes >> 20
        );
        assert!(memory_bytes > 0 && memory_bytes < 256 << 20);
        assert!(vm_dir(&pod_id).join("snapshot/vmstate").exists());
        let pod = get_pod(&mut pods, &pod_id).await?;
        assert_eq!(pod.state(), PodState::Suspended);
        assert!(pod.microvm.is_none(), "A suspended pod has no Firecracker");

        // The guest's clock stands still in the snapshot.
        tokio::time::sleep(Duration::from_secs(3)).await;

        info!("Resuming pod {pod_id}");
        pods.resume_pod(ResumePodRequest {
            pod_id: pod_id.clone(),
        })
        .await?;
        let pod = get_pod(&mut pods, &pod_id).await?;
        assert_eq!(pod.state(), PodState::Running);
        assert!(
            !vm_dir(&pod_id).join("snapshot").exists(),
            "A resumed pod's snapshot is gone"
        );
        let pid = pod.microvm.context("a running pod has a microVM")?.pid;
        guard.set_pid(pid as i32);
        let peak = check_cgroup(&pod_id, pid, 1)?;
        info!("The pod's cgroup accounted at most {peak} MiB, its snapshot included");
        let owner =
            std::fs::read_to_string(Path::new("/sys/class/net").join(&tap_name).join("owner"))?;
        assert_eq!(owner.trim(), process_uid(pid)?.to_string());

        // The same boot, with its memory: the ping count goes on.
        let pong = guest_ping(&pod_id).await?;
        assert_eq!(pong.boot_id, first.boot_id);
        assert!(pong.tick > first.tick, "The guest forgot its pings");
        let skew = guest_clock_skew(&pod_id).await?;
        assert!(
            skew < Duration::from_secs(2),
            "The guest's clock is {skew:?} off"
        );
    }

    info!("Suspending pod {pod_id} and stopping it: the snapshot goes");
    pods.suspend_pod(SuspendPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    pods.stop_pod(StopPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    assert!(!vm_dir(&pod_id).exists(), "A stopped pod keeps no snapshot");
    pods.start_pod(StartPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let pod = wait_for_pod(&mut pods, &pod_id, PodState::Running).await?;
    guard.set_pid(pod.microvm.context("a running pod has a microVM")?.pid as i32);
    assert_ne!(guest_ping(&pod_id).await?.boot_id, first.boot_id);

    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    verify_microvm_cleanup(&pod_id);
    assert!(
        !Path::new("/sys/class/net").join(&tap_name).exists(),
        "The pod's TAP device is gone with the pod"
    );
    guard.disable_cleanup();
    Ok(())
}
