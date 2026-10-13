// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use feos_proto::{
    container_service::container_service_client::ContainerServiceClient,
    host_service::host_service_client::HostServiceClient,
    image_service::image_service_client::ImageServiceClient,
    vm_service::vm_service_client::VmServiceClient,
};
use hyper_util::rt::TokioIo;
use image_service::IMAGE_SERVICE_SOCKET;
use log::{error, info};
use nix::sys::prctl;
use std::env;
use std::process::Command;
use std::sync::Arc;
use std::sync::{LazyLock as Lazy, OnceLock as SyncOnceCell};
use std::time::Duration;
use tokio::net::UnixStream;
use tokio::sync::OnceCell as TokioOnceCell;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;
use vm_service::FIRECRACKER_BIN;

pub mod container_tests;
pub mod fixtures;
pub mod host_tests;
pub mod image_tests;
pub mod vm_tests;

pub const PUBLIC_SERVER_ADDRESS: &str = "http://[::1]:1337";
pub const DEFAULT_TEST_CONTAINER_IMAGE_REF: &str = "docker.io/library/busybox:1.36";

/// The FeOS guest as a VM image: the VM tests talk to its API over vsock and
/// shut it down with Ctrl-Alt-Del. `make test-vm` pushes one to a local
/// registry and runs the VM tests against it.
pub static TEST_IMAGE_REF: Lazy<Option<String>> =
    Lazy::new(|| env::var("TEST_IMAGE_REF").ok().filter(|r| !r.is_empty()));
pub static TEST_CONTAINER_IMAGE_REF: Lazy<String> = Lazy::new(|| {
    env::var("TEST_CONTAINER_IMAGE_REF")
        .unwrap_or_else(|_| DEFAULT_TEST_CONTAINER_IMAGE_REF.to_string())
});

static SERVER_RUNTIME: TokioOnceCell<Arc<tokio::runtime::Runtime>> = TokioOnceCell::const_new();
static TEMP_DIR_GUARD: SyncOnceCell<tempfile::TempDir> = SyncOnceCell::new();

pub async fn ensure_server() {
    SERVER_RUNTIME
        .get_or_init(|| async { setup_server().await })
        .await;
}

async fn setup_server() -> Arc<tokio::runtime::Runtime> {
    let temp_dir = TEMP_DIR_GUARD.get_or_init(|| {
        tempfile::Builder::new()
            .prefix("feos-test-")
            .tempdir()
            .expect("Failed to create temp dir")
    });

    let db_path = temp_dir.path().join("vms.db");
    let db_url = format!("sqlite:{}", db_path.to_str().unwrap());

    // SAFETY: not strictly sound: the test harness runs tests on several
    // threads, and another thread may read the environment concurrently.
    // TODO: pass the database URLs to run_server instead of the environment.
    unsafe {
        env::set_var("DATABASE_URL", &db_url);
        // FeOS runs inside this test binary; the task service needs the real one.
        env::set_var("FEOS_TASK_SERVICE_BINARY", env!("CARGO_BIN_EXE_feos"));
    }
    info!("Using temporary database for tests: {db_url}");

    if let Err(e) = prctl::set_child_subreaper(true) {
        log::error!("Failed to set as child subreaper, container tests will fail: {e}");
    }

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("Failed to create a new Tokio runtime for the server");

    runtime.spawn(async move {
        if let Err(e) = main_server::run_server(false).await {
            panic!("Test server failed to run: {e}");
        }
    });

    info!("Waiting for the server to start...");
    for _ in 0..60 {
        if Channel::from_static(PUBLIC_SERVER_ADDRESS)
            .connect()
            .await
            .is_ok()
        {
            info!("Server is up and running at {PUBLIC_SERVER_ADDRESS}");
            return Arc::new(runtime);
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    error!("Server did not start in time.");
    panic!("Server did not start in time.");
}

pub async fn get_public_clients() -> Result<(
    VmServiceClient<Channel>,
    HostServiceClient<Channel>,
    ContainerServiceClient<Channel>,
)> {
    let vm_client = VmServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let host_client = HostServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let container_client = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    Ok((vm_client, host_client, container_client))
}

pub async fn get_image_service_client() -> Result<ImageServiceClient<Channel>> {
    let endpoint = Endpoint::from_static("http://[::1]:50051");
    let channel = endpoint
        .connect_with_connector(service_fn(|_: Uri| async {
            UnixStream::connect(IMAGE_SERVICE_SOCKET)
                .await
                .map(TokioIo::new)
        }))
        .await?;
    Ok(ImageServiceClient::new(channel))
}

pub fn check_firecracker_binary() -> bool {
    Command::new("which")
        .arg(FIRECRACKER_BIN)
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

/// The VM image to test with, unless the test has to be skipped.
pub fn vm_test_image() -> Option<String> {
    if !check_firecracker_binary() {
        log::warn!("Skipping test because '{FIRECRACKER_BIN}' binary was not found in PATH.");
        return None;
    }
    if TEST_IMAGE_REF.is_none() {
        log::warn!("Skipping test because TEST_IMAGE_REF names no VM image.");
    }
    TEST_IMAGE_REF.clone()
}
