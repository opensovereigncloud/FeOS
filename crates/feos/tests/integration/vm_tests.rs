// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use super::fixtures::{verify_vm_socket_cleanup, wait_for_target_state, VmGuard};
use super::{ensure_server, get_public_clients, vm_test_image};
use anyhow::{Context, Result};
use feos_proto::host_service::{
    host_service_client::HostServiceClient, GetVersionInfoRequest, PingRequest,
};
use feos_proto::vm_service::{
    stream_vm_console_request as console_input, AttachConsoleMessage, CpuConfig, CreateVmRequest,
    DeleteVmRequest, DiskConfig, GetVmRequest, MemoryConfig, PauseVmRequest, PingVmRequest,
    ResumeVmRequest, ShutdownVmRequest, StartVmRequest, StreamVmConsoleRequest,
    StreamVmEventsRequest, VmConfig, VmState,
};
use feos_utils::vsock::API_PORT;
use log::info;
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use std::time::Duration;
use tokio::time::timeout;
use tokio_stream::StreamExt;
use vm_service::vm_dir;

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

    let vm_config = VmConfig {
        cpus: Some(CpuConfig { vcpu_count: 2 }),
        memory: Some(MemoryConfig {
            size_mib: 512,
            hugepages: false,
        }),
        image_ref,
        disks: vec![DiskConfig {
            path: disk.to_string_lossy().into_owned(),
            ..Default::default()
        }],
        net: vec![],
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

    info!("Pinging VMM for vm_id: {vm_id}");
    let ping_req = PingVmRequest {
        vm_id: vm_id.clone(),
    };
    let ping_res = vm_client.ping_vm(ping_req).await?.into_inner();
    info!("VMM Ping successful, PID: {}", ping_res.pid);
    guard.set_pid(ping_res.pid as i32);

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

    let vm_config = VmConfig {
        cpus: Some(CpuConfig { vcpu_count: 1 }),
        memory: Some(MemoryConfig {
            size_mib: 256,
            hugepages: false,
        }),
        image_ref,
        disks: vec![],
        net: vec![],
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
