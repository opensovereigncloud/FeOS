// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use super::fixtures::{
    verify_vm_socket_cleanup, wait_for_guest_attached, wait_for_target_state, LoopDevice, VmGuard,
};
use super::{ensure_server, get_image_service_client, get_public_clients, vm_test_image};
use anyhow::{Context, Result};
use feos_proto::host_service::{
    host_service_client::HostServiceClient, GetNetworkConfigRequest, GetTimeRequest,
    GetVersionInfoRequest, PingRequest,
};
use feos_proto::image_service::DeleteImageRequest;
use feos_proto::vm_service::{
    stream_vm_console_request as console_input, AttachConsoleMessage, BalloonConfig, CpuConfig,
    CreateVmRequest, DeleteVmRequest, DiskConfig, GetBalloonRequest, GetVmRequest,
    GuestNetworkConfig, MemoryConfig, NetConfig, PauseVmRequest, PingVmRequest, ResumeVmRequest,
    ShutdownVmRequest, StartVmRequest, StreamVmConsoleRequest, StreamVmEventsRequest,
    SuspendVmRequest, TapConfig, UpdateBalloonRequest, VmConfig, VmState,
};
use feos_utils::vsock::API_PORT;
use log::info;
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::Path;
use std::time::{Duration, SystemTime};
use tokio::time::timeout;
use tokio_stream::StreamExt;
use vm_service::vm_dir;
use vm_service::vmm::jail::{self, tap};

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

/// The directory of the image whose kernel the VM's jail links.
fn vm_image_dir(vm_id: &str) -> Result<std::path::PathBuf> {
    let kernel = std::fs::metadata(vm_dir(vm_id).join("vmlinuz"))?;
    std::fs::read_dir(image_service::IMAGE_DIR)?
        .flatten()
        .map(|entry| entry.path())
        .find(|dir| {
            std::fs::metadata(dir.join("vmlinuz"))
                .is_ok_and(|meta| meta.dev() == kernel.dev() && meta.ino() == kernel.ino())
        })
        .context("No image has the VM's kernel")
}

/// Checks that the VM's Firecracker runs in the VM's cgroup, with the
/// VM's share of the CPU, and returns the most memory the cgroup has
/// accounted, in MiB.
fn check_cgroup(vm_id: &str, pid: i64, vcpus: u32) -> Result<u64> {
    let cgroup = jail::cgroup(vm_id);
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

#[tokio::test]
async fn test_create_and_start_vm() -> Result<()> {
    let Some(image_ref) = vm_test_image() else {
        return Ok(());
    };

    ensure_server().await;
    let (mut vm_client, _, _) = get_public_clients().await?;

    // An empty data disk: the guest finds it, fails to mount it, and says so.
    let disk_dir = tempfile::tempdir()?;
    let disk = disk_dir.path().join("data.img");
    std::fs::File::create(&disk)?.set_len(64 << 20)?;
    // The VM service creates the TAP device and hands it to the VM's user.
    let tap_name = format!("fct{}", std::process::id());

    let vm_config = VmConfig {
        cpus: Some(CpuConfig { vcpu_count: 2 }),
        memory: Some(MemoryConfig {
            size_mib: 512,
            hugepages: false,
            balloon: Some(BalloonConfig {
                amount_mib: 0,
                deflate_on_oom: true,
                free_page_reporting: true,
                stats_interval_s: 1,
            }),
        }),
        image_ref,
        disks: vec![DiskConfig {
            path: disk.to_string_lossy().into_owned(),
            ..Default::default()
        }],
        net: vec![NetConfig {
            tap: Some(TapConfig {
                tap_name: tap_name.clone(),
            }),
            ..Default::default()
        }],
        guest_network: Some(GuestNetworkConfig {
            interface: "eth0".to_string(),
            ipv4_cidr: "10.99.0.2/24".to_string(),
            gateway: "10.99.0.1".to_string(),
            dns: vec!["10.99.0.53".to_string()],
            hostname: "feos-test".to_string(),
        }),
    };
    let create_req = CreateVmRequest {
        config: Some(vm_config),
        vm_id: None,
    };

    info!("Sending CreateVm request");
    let create_res = vm_client.create_vm(create_req).await?.into_inner();
    let vm_id = create_res.vm_id;
    info!("VM created with ID: {vm_id}");

    let mut guard = VmGuard::new(vm_id.clone());

    info!("Immediately calling StartVm for vm_id: {vm_id}, expecting error");
    let start_req = StartVmRequest {
        vm_id: vm_id.clone(),
    };
    let result = vm_client.start_vm(start_req.clone()).await;
    assert!(result.is_err(), "StartVm should fail when VM is Creating");

    info!("Connecting to StreamVmEvents stream for vm_id: {vm_id}");
    let events_req = StreamVmEventsRequest {
        vm_id: Some(vm_id.clone()),
        ..Default::default()
    };
    let mut stream = vm_client.stream_vm_events(events_req).await?.into_inner();

    timeout(
        Duration::from_secs(180),
        wait_for_target_state(&mut stream, VmState::Created),
    )
    .await
    .expect("Timed out waiting for VM to become created")?;
    info!("VM is in CREATED state");

    info!("Sending StartVm request for vm_id: {vm_id}");
    vm_client.start_vm(start_req.clone()).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Running),
    )
    .await
    .expect("Timed out waiting for VM to become running")?;
    info!("VM is in RUNNING state");

    let guest = timeout(
        Duration::from_secs(30),
        wait_for_guest_attached(&mut stream),
    )
    .await
    .expect("Timed out waiting for the guest to attach")?;
    assert_eq!(guest.attach_seq, 0, "A boot's first attach");
    assert!(
        guest.push_error.is_empty(),
        "Push failed: {}",
        guest.push_error
    );
    let first_boot = guest.boot_id;

    // The guest has no RTC and no NTP: its clock and its network are what
    // the VM service pushed when it attached.
    let vsock_path = vm_client
        .get_vm(GetVmRequest {
            vm_id: vm_id.clone(),
        })
        .await?
        .into_inner()
        .vsock_path;
    let mut guest_host =
        HostServiceClient::new(feos_utils::vsock::channel(&vsock_path, API_PORT).await?);
    let network = guest_host
        .get_network_config(GetNetworkConfigRequest {})
        .await?
        .into_inner();
    assert!(network.configured);
    assert_eq!(network.interface, "eth0");
    assert_eq!(network.ipv4_cidr, "10.99.0.2/24");
    assert_eq!(network.dns, vec!["10.99.0.53".to_string()]);
    assert_eq!(network.hostname, "feos-test");
    let guest_time: SystemTime = guest_host
        .get_time(GetTimeRequest {})
        .await?
        .into_inner()
        .time
        .context("GetTime returned no time")?
        .try_into()?;
    let skew = SystemTime::now()
        .duration_since(guest_time)
        .unwrap_or_else(|e| e.duration());
    assert!(
        skew < Duration::from_secs(5),
        "The guest's clock is {skew:?} off"
    );
    info!("The guest got its network and its clock, {skew:?} off");

    for amount_mib in [64, 0] {
        info!("Setting the balloon to {amount_mib} MiB");
        vm_client
            .update_balloon(UpdateBalloonRequest {
                vm_id: vm_id.clone(),
                amount_mib,
            })
            .await?;
        let balloon = timeout(Duration::from_secs(15), async {
            loop {
                let balloon = vm_client
                    .get_balloon(GetBalloonRequest {
                        vm_id: vm_id.clone(),
                    })
                    .await?
                    .into_inner();
                if balloon.actual_mib == amount_mib {
                    return anyhow::Ok(balloon);
                }
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        })
        .await
        .expect("Timed out waiting for the balloon")?;
        assert_eq!(balloon.target_mib, amount_mib);
        let stats = balloon.stats.context("The balloon has statistics")?;
        assert!(stats.total_memory > 0);
    }

    info!("Sending ShutdownVm request for vm_id: {vm_id}");
    let shutdown_req = ShutdownVmRequest {
        vm_id: vm_id.clone(),
    };
    vm_client.shutdown_vm(shutdown_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Stopped),
    )
    .await
    .expect("Timed out waiting for VM to become stopped")?;
    info!("VM is in STOPPED state");

    info!("Calling ResumeVm in Stopped state for vm_id: {vm_id}, expecting error");
    let resume_req = ResumeVmRequest {
        vm_id: vm_id.clone(),
    };
    let result = vm_client.resume_vm(resume_req.clone()).await;
    assert!(result.is_err(), "ResumeVm should fail when VM is Stopped");

    info!("Calling StreamVmConsole in Stopped state for vm_id: {vm_id}, expecting error");
    let (console_tx, console_rx) = tokio::sync::mpsc::channel(1);
    let console_stream = tokio_stream::wrappers::ReceiverStream::new(console_rx);

    let attach_payload = console_input::Payload::Attach(AttachConsoleMessage {
        vm_id: vm_id.clone(),
    });
    let attach_input = StreamVmConsoleRequest {
        payload: Some(attach_payload),
    };

    console_tx
        .send(attach_input)
        .await
        .expect("Failed to send attach message");

    let response = vm_client.stream_vm_console(console_stream).await;
    assert!(
        response.is_ok(),
        "StreamVmConsole should establish stream successfully"
    );

    let mut output_stream = response.unwrap().into_inner();

    let stream_result = output_stream.next().await;
    match stream_result {
        Some(Err(status)) => {
            info!(
                "Received expected error from console stream: {}",
                status.message()
            );
            assert!(
                status.message().contains("Invalid VM state")
                    || status.message().contains("Stopped"),
                "Error should be about invalid VM state, got: {}",
                status.message()
            );
        }
        Some(Ok(_)) => {
            panic!("StreamVmConsole should fail when VM is Stopped, but got success response")
        }
        None => panic!("StreamVmConsole stream ended unexpectedly without error"),
    }

    info!("Sending StartVm request again for vm_id: {vm_id}");
    vm_client.start_vm(start_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Running),
    )
    .await
    .expect("Timed out waiting for VM to become running again")?;
    info!("VM is in RUNNING state again");

    let guest = timeout(
        Duration::from_secs(30),
        wait_for_guest_attached(&mut stream),
    )
    .await
    .expect("Timed out waiting for the guest to attach again")?;
    assert_ne!(guest.boot_id, first_boot, "A new boot has a new boot ID");
    assert_eq!(guest.attach_seq, 0);

    info!("Sending PauseVm request for vm_id: {vm_id}");
    let pause_req = PauseVmRequest {
        vm_id: vm_id.clone(),
    };
    vm_client.pause_vm(pause_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Paused),
    )
    .await
    .expect("Timed out waiting for VM to become paused")?;
    info!("VM is in PAUSED state");

    info!("Calling StartVm in Paused state for vm_id: {vm_id}, expecting error");
    let start_req_paused = StartVmRequest {
        vm_id: vm_id.clone(),
    };
    let result = vm_client.start_vm(start_req_paused).await;
    assert!(result.is_err(), "StartVm should fail when VM is Paused");

    info!("Sending ResumeVm request for vm_id: {vm_id}");
    vm_client.resume_vm(resume_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Running),
    )
    .await
    .expect("Timed out waiting for VM to become running after resume")?;
    info!("VM is in RUNNING state after resume");

    let get_req = GetVmRequest {
        vm_id: vm_id.clone(),
    };
    let info_res = vm_client.get_vm(get_req).await?.into_inner();
    assert_eq!(
        VmState::try_from(info_res.state).unwrap(),
        VmState::Running,
        "VM state from GetVm should be RUNNING"
    );
    let guest = info_res.guest.clone().context("GetVm reports no guest")?;
    assert!(guest.attached);
    assert!(
        guest.push_error.is_empty(),
        "Push failed: {}",
        guest.push_error
    );

    info!("Pinging VMM for vm_id: {vm_id}");
    let ping_req = PingVmRequest {
        vm_id: vm_id.clone(),
    };
    let ping_res = vm_client.ping_vm(ping_req).await?.into_inner();
    info!("VMM Ping successful, PID: {}", ping_res.pid);
    guard.set_pid(ping_res.pid as i32);

    // Firecracker runs jailed, as a user of the VM's own, in a cgroup of
    // the VM's own.
    assert_ne!(process_uid(ping_res.pid)?, 0, "Firecracker runs as root");
    let peak = check_cgroup(&vm_id, ping_res.pid, 2)?;
    info!("The VM's cgroup accounted at most {peak} MiB");

    // The jail links the files of the VM's image, which stays as long as
    // the VM is there.
    let image_dir = vm_image_dir(&vm_id)?;
    let status = get_image_service_client()
        .await?
        .delete_image(DeleteImageRequest {
            image_uuid: image_dir
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .into_owned(),
        })
        .await
        .expect_err("The image of a VM cannot be deleted");
    assert_eq!(status.code(), tonic::Code::FailedPrecondition);
    info!("The VM's image is in use: {}", status.message());

    // The guest is FeOS: its API answers behind the VM's vsock.
    let vsock_path = info_res.vsock_path;
    info!("Pinging the guest through {vsock_path}");
    let guest_ping = timeout(Duration::from_secs(30), async {
        loop {
            if let Ok(channel) = feos_utils::vsock::channel(&vsock_path, API_PORT).await
                && let Ok(ping) = HostServiceClient::new(channel).ping(PingRequest {}).await
            {
                return ping.into_inner();
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    })
    .await
    .expect("Timed out waiting for the guest's API");
    info!("The guest answered, uptime {}s", guest_ping.uptime_seconds);

    let serial_log = std::fs::read_to_string(vm_dir(&vm_id).join("serial.log"))?;
    assert!(
        serial_log.contains("data volume /dev/vda"),
        "The guest should have found its data disk"
    );

    info!("Reading the console while the guest logs a call");
    let (console_tx, console_rx) = tokio::sync::mpsc::channel(1);
    console_tx
        .send(StreamVmConsoleRequest {
            payload: Some(console_input::Payload::Attach(AttachConsoleMessage {
                vm_id: vm_id.clone(),
            })),
        })
        .await?;
    let mut console = vm_client
        .stream_vm_console(tokio_stream::wrappers::ReceiverStream::new(console_rx))
        .await?
        .into_inner();
    // The console shows output from the moment it is attached.
    tokio::time::sleep(Duration::from_millis(200)).await;
    let guest = feos_utils::vsock::channel(&vsock_path, API_PORT).await?;
    HostServiceClient::new(guest)
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

    info!("Deleting VM: {vm_id}");
    let delete_req = DeleteVmRequest {
        vm_id: vm_id.clone(),
    };
    vm_client.delete_vm(delete_req).await?.into_inner();
    info!("DeleteVm call successful");

    verify_vm_socket_cleanup(&vm_id);
    assert!(
        !Path::new("/sys/class/net").join(&tap_name).exists(),
        "The TAP device the VM service created is gone with the VM"
    );
    assert!(
        !jail::cgroup(&vm_id).exists(),
        "The VM's cgroup is gone with the VM"
    );
    assert!(!image_dir.exists(), "The VM's image is gone with the VM");

    guard.disable_cleanup();
    Ok(())
}

#[tokio::test]
async fn test_vm_healthcheck_and_crash_recovery() -> Result<()> {
    let Some(image_ref) = vm_test_image() else {
        return Ok(());
    };

    ensure_server().await;
    let (mut vm_client, _, _) = get_public_clients().await?;

    // A data disk that is a block device: the jail gets a node of its own.
    let disk_dir = tempfile::tempdir()?;
    let disk_file = disk_dir.path().join("data.img");
    std::fs::File::create(&disk_file)?.set_len(64 << 20)?;
    let disk = LoopDevice::new(&disk_file)?;

    let vm_config = VmConfig {
        cpus: Some(CpuConfig { vcpu_count: 1 }),
        memory: Some(MemoryConfig {
            size_mib: 256,
            hugepages: false,
            balloon: None,
        }),
        image_ref,
        disks: vec![DiskConfig {
            path: disk.path.to_string_lossy().into_owned(),
            ..Default::default()
        }],
        net: vec![],
        guest_network: None,
    };
    let create_req = CreateVmRequest {
        config: Some(vm_config),
        vm_id: None,
    };

    info!("Sending CreateVm request for healthcheck test");
    let create_res = vm_client.create_vm(create_req).await?.into_inner();
    let vm_id = create_res.vm_id;
    info!("VM created with ID: {vm_id}");

    let mut guard = VmGuard::new(vm_id.clone());

    info!("Connecting to StreamVmEvents stream for vm_id: {vm_id}");
    let events_req = StreamVmEventsRequest {
        vm_id: Some(vm_id.clone()),
        ..Default::default()
    };
    let mut stream = vm_client.stream_vm_events(events_req).await?.into_inner();

    timeout(
        Duration::from_secs(180),
        wait_for_target_state(&mut stream, VmState::Created),
    )
    .await
    .expect("Timed out waiting for VM to become created")?;
    info!("VM is in CREATED state");

    info!("Sending StartVm request for vm_id: {vm_id}");
    let start_req = StartVmRequest {
        vm_id: vm_id.clone(),
    };
    vm_client.start_vm(start_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Running),
    )
    .await
    .expect("Timed out waiting for VM to become running")?;
    info!("VM is in RUNNING state");

    info!("Pinging VMM for vm_id: {vm_id}");
    let ping_req = PingVmRequest {
        vm_id: vm_id.clone(),
    };
    let ping_res = vm_client.ping_vm(ping_req).await?.into_inner();
    info!("VMM Ping successful, PID: {}", ping_res.pid);
    let pid_to_kill = Pid::from_raw(ping_res.pid as i32);
    guard.set_pid(ping_res.pid as i32);

    let node = std::fs::metadata(vm_dir(&vm_id).join("disk0.img"))?;
    assert!(node.file_type().is_block_device());
    assert_eq!(node.rdev(), std::fs::metadata(&disk.path)?.rdev());
    assert_eq!(node.uid(), process_uid(ping_res.pid)?);
    timeout(Duration::from_secs(30), async {
        while !std::fs::read_to_string(vm_dir(&vm_id).join("serial.log"))
            .unwrap_or_default()
            .contains("data volume /dev/vda")
        {
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    })
    .await
    .expect("The guest did not find its data disk");
    info!("The guest found its data disk, {}", disk.path.display());

    info!("Forcefully killing hypervisor process with PID: {pid_to_kill}");
    kill(pid_to_kill, Signal::SIGKILL).context("Failed to kill hypervisor process")?;
    info!("Successfully sent SIGKILL to process {pid_to_kill}");

    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Crashed),
    )
    .await
    .expect("Timed out waiting for VM to enter Crashed state")?;
    info!("VM is in CRASHED state as expected");

    info!("Verifying API calls fail for crashed VM: {vm_id}");

    let start_req = StartVmRequest {
        vm_id: vm_id.clone(),
    };
    assert!(
        vm_client.start_vm(start_req).await.is_err(),
        "StartVm should fail for a crashed VM"
    );

    let pause_req = PauseVmRequest {
        vm_id: vm_id.clone(),
    };
    assert!(
        vm_client.pause_vm(pause_req).await.is_err(),
        "PauseVm should fail for a crashed VM"
    );

    let shutdown_req = ShutdownVmRequest {
        vm_id: vm_id.clone(),
    };
    assert!(
        vm_client.shutdown_vm(shutdown_req).await.is_err(),
        "ShutdownVm should fail for a crashed VM"
    );

    let resume_req = ResumeVmRequest {
        vm_id: vm_id.clone(),
    };
    assert!(
        vm_client.resume_vm(resume_req).await.is_err(),
        "ResumeVm should fail for a crashed VM"
    );

    info!("API call failure checks passed for crashed VM");

    info!("Deleting crashed VM: {vm_id}");
    let delete_req = DeleteVmRequest {
        vm_id: vm_id.clone(),
    };
    vm_client.delete_vm(delete_req).await?.into_inner();
    info!("DeleteVm call successful for crashed VM");

    verify_vm_socket_cleanup(&vm_id);

    guard.disable_cleanup();
    Ok(())
}

#[tokio::test]
async fn test_suspend_and_restore_vm() -> Result<()> {
    let Some(image_ref) = vm_test_image() else {
        return Ok(());
    };

    ensure_server().await;
    let (mut vm_client, _, _) = get_public_clients().await?;

    // A TAP device of the caller's: the VM service hands it to the VM's
    // user, and leaves it when the VM goes.
    let tap_name = format!("fcs{}", std::process::id());
    tap::ensure(&tap_name, 0)?;

    let vm_config = VmConfig {
        cpus: Some(CpuConfig { vcpu_count: 1 }),
        memory: Some(MemoryConfig {
            size_mib: 256,
            hugepages: false,
            balloon: Some(BalloonConfig {
                amount_mib: 0,
                deflate_on_oom: true,
                free_page_reporting: true,
                stats_interval_s: 0,
            }),
        }),
        image_ref,
        disks: vec![],
        net: vec![NetConfig {
            tap: Some(TapConfig {
                tap_name: tap_name.clone(),
            }),
            ..Default::default()
        }],
        guest_network: None,
    };
    let vm_id = vm_client
        .create_vm(CreateVmRequest {
            config: Some(vm_config),
            vm_id: None,
        })
        .await?
        .into_inner()
        .vm_id;
    info!("VM created with ID: {vm_id}");
    let mut guard = VmGuard::new(vm_id.clone());

    let mut stream = vm_client
        .stream_vm_events(StreamVmEventsRequest {
            vm_id: Some(vm_id.clone()),
            ..Default::default()
        })
        .await?
        .into_inner();
    timeout(
        Duration::from_secs(180),
        wait_for_target_state(&mut stream, VmState::Created),
    )
    .await
    .expect("Timed out waiting for VM to become created")?;

    let start_req = StartVmRequest {
        vm_id: vm_id.clone(),
    };
    vm_client.start_vm(start_req.clone()).await?;
    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Running),
    )
    .await
    .expect("Timed out waiting for VM to become running")?;
    let boot_id = timeout(
        Duration::from_secs(30),
        wait_for_guest_attached(&mut stream),
    )
    .await
    .expect("Timed out waiting for the guest to attach")?
    .boot_id;

    let vsock_path = vm_client
        .get_vm(GetVmRequest {
            vm_id: vm_id.clone(),
        })
        .await?
        .into_inner()
        .vsock_path;
    let ping = |vsock_path: String| async move {
        let channel = feos_utils::vsock::channel(&vsock_path, API_PORT).await?;
        anyhow::Ok(
            HostServiceClient::new(channel)
                .ping(PingRequest {})
                .await?
                .into_inner(),
        )
    };
    let mut tick = ping(vsock_path.clone()).await?.tick;

    for purge_guest in [true, false] {
        info!("Suspending VM {vm_id} (purge: {purge_guest})");
        let memory_bytes = vm_client
            .suspend_vm(SuspendVmRequest {
                vm_id: vm_id.clone(),
                purge_guest,
            })
            .await?
            .into_inner()
            .memory_bytes;
        timeout(
            Duration::from_secs(30),
            wait_for_target_state(&mut stream, VmState::Suspended),
        )
        .await
        .expect("Timed out waiting for VM to become suspended")?;
        info!(
            "VM is suspended; its memory takes {} MiB",
            memory_bytes >> 20
        );
        assert!(memory_bytes > 0 && memory_bytes < 256 << 20);
        assert!(vm_dir(&vm_id).join("snapshot/vmstate").exists());
        assert!(
            vm_client
                .ping_vm(PingVmRequest {
                    vm_id: vm_id.clone(),
                })
                .await
                .is_err(),
            "A suspended VM has no VMM"
        );

        // The guest's clock stands still in the snapshot.
        tokio::time::sleep(Duration::from_secs(3)).await;

        info!("Restoring VM {vm_id}");
        vm_client
            .resume_vm(ResumeVmRequest {
                vm_id: vm_id.clone(),
            })
            .await?;
        timeout(
            Duration::from_secs(30),
            wait_for_target_state(&mut stream, VmState::Running),
        )
        .await
        .expect("Timed out waiting for VM to run again")?;
        assert!(
            !vm_dir(&vm_id).join("snapshot").exists(),
            "A restored VM's snapshot is gone"
        );
        let pid = vm_client
            .ping_vm(PingVmRequest {
                vm_id: vm_id.clone(),
            })
            .await?
            .into_inner()
            .pid;
        let peak = check_cgroup(&vm_id, pid, 1)?;
        info!("The VM's cgroup accounted at most {peak} MiB, its snapshot included");
        let owner =
            std::fs::read_to_string(Path::new("/sys/class/net").join(&tap_name).join("owner"))?;
        assert_eq!(owner.trim(), process_uid(pid)?.to_string());

        // The same boot, with its memory: the ping count goes on.
        let pong = ping(vsock_path.clone()).await?;
        assert_eq!(pong.boot_id, boot_id);
        assert!(pong.tick > tick, "The guest forgot its pings");
        tick = pong.tick;

        let guest = vm_client
            .get_vm(GetVmRequest {
                vm_id: vm_id.clone(),
            })
            .await?
            .into_inner()
            .guest
            .context("GetVm reports no guest")?;
        assert!(
            guest.push_error.is_empty(),
            "Push failed: {}",
            guest.push_error
        );
        let guest_time: SystemTime =
            HostServiceClient::new(feos_utils::vsock::channel(&vsock_path, API_PORT).await?)
                .get_time(GetTimeRequest {})
                .await?
                .into_inner()
                .time
                .context("GetTime returned no time")?
                .try_into()?;
        let skew = SystemTime::now()
            .duration_since(guest_time)
            .unwrap_or_else(|e| e.duration());
        assert!(
            skew < Duration::from_secs(2),
            "The guest's clock is {skew:?} off"
        );
    }

    info!("Suspending VM {vm_id} and booting it instead of restoring it");
    vm_client
        .suspend_vm(SuspendVmRequest {
            vm_id: vm_id.clone(),
            purge_guest: false,
        })
        .await?;
    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Suspended),
    )
    .await
    .expect("Timed out waiting for VM to become suspended")?;
    vm_client.start_vm(start_req).await?;
    timeout(
        Duration::from_secs(30),
        wait_for_target_state(&mut stream, VmState::Running),
    )
    .await
    .expect("Timed out waiting for VM to boot")?;
    let guest = timeout(
        Duration::from_secs(30),
        wait_for_guest_attached(&mut stream),
    )
    .await
    .expect("Timed out waiting for the guest to attach")?;
    assert_ne!(guest.boot_id, boot_id, "A boot discards the snapshot");
    assert!(!vm_dir(&vm_id).join("snapshot").exists());

    let ping_res = vm_client
        .ping_vm(PingVmRequest {
            vm_id: vm_id.clone(),
        })
        .await?
        .into_inner();
    guard.set_pid(ping_res.pid as i32);

    vm_client
        .delete_vm(DeleteVmRequest {
            vm_id: vm_id.clone(),
        })
        .await?;
    verify_vm_socket_cleanup(&vm_id);
    assert!(
        Path::new("/sys/class/net").join(&tap_name).exists(),
        "The caller's TAP device outlives the VM"
    );
    tap::delete(&tap_name)?;
    guard.disable_cleanup();
    Ok(())
}
