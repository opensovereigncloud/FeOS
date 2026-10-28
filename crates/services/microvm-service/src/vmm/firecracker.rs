// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! VMs as Firecracker processes. Each VM has a directory of its own, the
//! chroot of its jail (see `jail`):
//!
//! - `config.json`: its Firecracker configuration, written at creation
//! - `metadata.json`: what its metadata service serves the guest, written
//!   at creation, if it has one
//! - `fc.sock`: Firecracker's API socket
//! - `v.sock`: the host end of the guest's vsock device
//! - `console.in`: a FIFO, the serial console's input (Firecracker's stdin)
//! - `serial.log`: the serial console's output since the last boot
//!   (Firecracker's stdout)
//! - `vmm.log`: Firecracker's own log
//! - `firecracker.pid`, `firecracker` and `dev/`: the jailer's
//! - `vmlinuz`, `initramfs`: the guest's files, linked in
//! - `data`: the data disk, a device node of the VM's own or a file
//!   linked in
//! - `volume0`, `volume1`, ...: the pod's volumes, the same way
//! - `snapshot/`: the snapshot of a suspended VM (see `snapshot`)
//! - `memory.sock`: where a userfaultfd handler serves the memory of a
//!   snapshot written elsewhere
//!
//! Creating a VM writes its configuration. Starting it starts a Firecracker
//! process that boots it at once. The process ends when the guest reboots or
//! shuts down, so every boot is a new process. Suspending a VM writes a
//! snapshot and ends the process; restoring it starts a process that loads
//! the snapshot, and puts the metadata in again, which a snapshot does not
//! hold.
//!
//! A suspend waits `SNAPSHOT_TIMEOUT` for the snapshot. Firecracker cannot
//! be stopped in the middle of one, and writing the memory of a VM that a
//! userfaultfd handler serves from the object store can take minutes: a
//! snapshot that is late fails its suspend at once, and Firecracker writes
//! on. Its files stay alone meanwhile, and the VM refuses other changes;
//! once Firecracker is done, the VM resumes if it ran and the snapshot is
//! discarded, so that the VM is as the suspend found it. A late snapshot
//! that makes no progress for `LATE_STALL` ends Firecracker, which the
//! health check reports as a crash.
//!
//! What removes a VM's files runs on the blocking pool, never on a thread
//! of the runtime: removing a memory file of GiBs takes its time, and one
//! that Firecracker still writes waits in the kernel until Firecracker is
//! done. A runtime thread blocked so stalls the whole runtime when it was
//! the one that drove its timers and its I/O.

use super::{fc_api, installed, jail, snapshot, Hypervisor, VmmError};
use crate::{jail_dir, vm_dir, vsock_path, VmEventWrapper, VM_DIR};
use feos_proto::microvm::{
    CreateVmRequest, DeleteVmRequest, DeleteVmResponse, PingVmRequest, PingVmResponse,
    ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, VmConfig, VmState, VmStateChangedEvent,
};
use feos_utils::pod_id::PodId;
use log::{info, warn};
use nix::sys::signal::{kill, Signal};
use nix::sys::stat::Mode;
use nix::unistd::{self, Pid};
use serde_json::{json, Value};
use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::io::{self, SeekFrom};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncSeekExt, AsyncWriteExt, DuplexStream};
use tokio::sync::{broadcast, mpsc, watch};
use tokio::task::JoinHandle;

/// `reboot=k`: a guest that reboots ends Firecracker, which cannot reboot
/// it. The i8042 options let Ctrl-Alt-Del through without probing for a
/// mouse or a multiplexer that Firecracker does not emulate.
const BOOT_ARGS: &str = "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.dumbkbd";
/// The guest's vsock CID. The host end is a Unix socket per VM, so all
/// guests can have the same.
const GUEST_CID: u32 = 3;
const DEFAULT_VCPUS: u32 = 1;
const DEFAULT_MEMORY_MIB: u64 = 512;
/// How long Firecracker may take to boot the VM and answer on its socket.
const BOOT_TIMEOUT: Duration = Duration::from_secs(10);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(30);
/// How long writing or loading a snapshot may take: the guest's memory goes
/// to disk and is synced. A suspend whose snapshot takes longer fails, and
/// Firecracker writes on (see the module's description).
const SNAPSHOT_TIMEOUT: Duration = Duration::from_secs(120);
/// How often a late snapshot is looked at, and how long it may make no
/// progress, its memory file not growing, before Firecracker is ended: a
/// VM whose memory does not come is stuck.
const LATE_POLL: Duration = Duration::from_secs(10);
const LATE_STALL: Duration = Duration::from_secs(300);
const CTRL_ALT_DEL_REPEAT: Duration = Duration::from_secs(3);
/// How long a killed Firecracker may take to go.
const KILL_TIMEOUT: Duration = Duration::from_secs(5);
const CONSOLE_POLL: Duration = Duration::from_millis(50);
/// How much of Firecracker's log an error carries.
const LOG_TAIL_LINES: usize = 5;
/// Files in the VM's directory, the jail's root.
const CONFIG: &str = "config.json";
const METADATA: &str = "metadata.json";
const API_SOCKET: &str = "fc.sock";
const VSOCK: &str = "v.sock";
const VMM_LOG: &str = "vmm.log";
/// The data disk in the jail.
const DATA_DISK: &str = "data";
/// A volume in the jail, and its drive: `volume<N>`.
const VOLUME_DISK: &str = "volume";

/// How a VMM process ended: its exit code, 128 + the signal when signalled,
/// or `None` when it was not this process's child and the code is unknown.
type Exit = Option<i32>;
/// `None` while the process runs.
type ExitWatch = watch::Receiver<Option<Exit>>;

pub struct FirecrackerAdapter {
    firecracker: PathBuf,
    jailer: PathBuf,
    /// The exits of the VMM processes this service watches, by VM ID.
    exits: Mutex<HashMap<String, ExitWatch>>,
    /// Serializes the allocation of the jails' user IDs.
    jails: Mutex<()>,
    /// The VMs whose late snapshot Firecracker writes on: they refuse
    /// other changes until it is done.
    late: Arc<Mutex<HashSet<String>>>,
}

impl FirecrackerAdapter {
    pub fn new(firecracker: PathBuf, jailer: PathBuf) -> Self {
        Self {
            firecracker,
            jailer,
            exits: Mutex::new(HashMap::new()),
            jails: Mutex::new(()),
            late: Arc::default(),
        }
    }

    fn exits(&self) -> MutexGuard<'_, HashMap<String, ExitWatch>> {
        // Every operation on the map is a single insert, lookup or removal.
        self.exits.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Marks the VM's snapshot late until the guard goes.
    fn mark_late(&self, vm_id: &str) -> LateMark {
        lock(&self.late).insert(vm_id.to_string());
        LateMark {
            late: self.late.clone(),
            vm_id: vm_id.to_string(),
        }
    }

    /// Watches `pid` until it exits; `child` is its handle if this process
    /// started it.
    fn watch_process(&self, vm_id: &str, pid: i32, child: Option<Child>) -> ExitWatch {
        let (tx, rx) = watch::channel(None);
        tokio::spawn(async move {
            let exit = wait_for_exit(pid, child).await;
            let _ = tx.send(Some(exit));
        });
        self.exits().insert(vm_id.to_string(), rx.clone());
        rx
    }

    /// The exit watch of the VM's VMM process, also for one this service
    /// did not start.
    fn exit_watch(&self, vm_id: &str) -> Result<ExitWatch, VmmError> {
        if let Some(rx) = self.exits().get(vm_id) {
            return Ok(rx.clone());
        }
        match read_pid(vm_id) {
            Some(pid) if self.is_vmm_process(vm_id, pid.into()) => {
                Ok(self.watch_process(vm_id, pid, None))
            }
            _ => Err(VmmError::VmNotFound(vm_id.to_string())),
        }
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

/// A VM's mark in `FirecrackerAdapter::late`, taken off when it goes.
struct LateMark {
    late: Arc<Mutex<HashSet<String>>>,
    vm_id: String,
}

impl Drop for LateMark {
    fn drop(&mut self) {
        lock(&self.late).remove(&self.vm_id);
    }
}

/// Runs `f` on the blocking pool: what removes or creates a VM's files.
async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> io::Result<T> + Send + 'static,
) -> io::Result<T> {
    tokio::task::spawn_blocking(f)
        .await
        .unwrap_or_else(|e| Err(io::Error::other(e)))
}

/// What Firecracker did with a snapshot it was asked for.
enum Written {
    /// It answered: the snapshot is written, or it failed. Either way
    /// Firecracker writes nothing more.
    Answered(Result<(), VmmError>),
    /// Not within the deadline. Firecracker writes on; the request answers
    /// once it is done.
    Late(JoinHandle<Result<Option<Value>, VmmError>>),
}

/// Asks the Firecracker behind `api` for a full snapshot and waits at most
/// `deadline` for it. The request runs in a task of its own, which outlives
/// the deadline.
async fn write_snapshot(api: &Path, deadline: Duration) -> Written {
    let api = api.to_path_buf();
    let body = json!({
        "snapshot_type": "Full",
        "snapshot_path": snapshot::JAILED_VMSTATE,
        "mem_file_path": snapshot::JAILED_MEMORY,
    });
    let mut create = tokio::spawn(async move {
        fc_api::request_unbounded(&api, "PUT", "/snapshot/create", Some(&body)).await
    });
    match tokio::time::timeout(deadline, &mut create).await {
        Ok(answer) => Written::Answered(match answer {
            Ok(answer) => answer.map(drop),
            Err(e) => Err(VmmError::Internal(format!("the snapshot's request: {e}"))),
        }),
        Err(_) => Written::Late(create),
    }
}

/// Resumes the paused VM behind `api`. One that does not resume is ended,
/// unless its Firecracker ended already: a VM left paused would hold its
/// pod frozen while its record says that it runs. The health check reports
/// the end as a crash.
async fn resume_or_end(vm_id: &str, pid: i32, api: &Path, exit: &ExitWatch) -> bool {
    let body = json!({"state": "Resumed"});
    match fc_api::request(api, "PATCH", "/vm", Some(&body)).await {
        Ok(_) => true,
        Err(e) => {
            if exit.borrow().is_none() {
                warn!(
                    "FirecrackerAdapter ({vm_id}): The VM did not resume ({e}); ending Firecracker \
                     (pid {pid})"
                );
                let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
            }
            false
        }
    }
}

/// Removes a snapshot that is not kept. What fails is logged: a boot
/// discards what is left.
async fn discard_snapshot(vm_id: &str, dir: &Path) {
    let dir = dir.to_path_buf();
    if let Err(e) = blocking(move || snapshot::remove_dir(&dir)).await {
        warn!("FirecrackerAdapter ({vm_id}): Failed to remove the snapshot: {e}");
    }
}

/// How a late snapshot ended.
#[derive(Debug, PartialEq)]
enum LateEnd {
    /// Firecracker finished it, and the VM runs again, or stays paused as
    /// the suspend found it.
    Finished,
    /// Firecracker finished it, and the VM did not resume: Firecracker was
    /// ended.
    NotResumed,
    /// Firecracker ended before it was done.
    Exited,
    /// It made no progress: Firecracker was ended.
    Stalled,
}

/// A snapshot that its suspend gave up on, which Firecracker writes on.
struct LateSnapshot {
    vm_id: String,
    pid: i32,
    api: PathBuf,
    /// The snapshot's directory, with its memory file.
    dir: PathBuf,
    /// The snapshot's request, which answers once Firecracker is done.
    create: JoinHandle<Result<Option<Value>, VmmError>>,
    exit: ExitWatch,
    /// The VM ran before the suspend paused it.
    resume: bool,
}

impl LateSnapshot {
    /// Waits until Firecracker is done with the snapshot, then leaves the
    /// VM as the suspend found it and discards the snapshot. A snapshot
    /// whose memory file does not grow for `stall`, looked at every `poll`,
    /// ends Firecracker.
    async fn wait_out(mut self, poll: Duration, stall: Duration) -> LateEnd {
        let vm_id = self.vm_id.clone();
        let started = tokio::time::Instant::now();
        let progress = stalled(self.dir.join("memory"), poll, stall);
        // Firecracker's answer, or how it ended without one.
        let answer = tokio::select! {
            answer = &mut self.create => Ok(answer),
            _ = self.exit.wait_for(Option::is_some) => Err(LateEnd::Exited),
            () = progress => Err(LateEnd::Stalled),
        };
        let end = match answer {
            Ok(answer) => {
                let how = match answer {
                    Ok(Ok(_)) => "written".to_string(),
                    Ok(Err(e)) => format!("failed: {e}"),
                    Err(e) => format!("lost: {e}"),
                };
                info!(
                    "FirecrackerAdapter ({vm_id}): Firecracker finished the late snapshot {}s \
                     after its suspend gave up ({how}); it is discarded",
                    started.elapsed().as_secs()
                );
                if !self.resume || resume_or_end(&vm_id, self.pid, &self.api, &self.exit).await {
                    LateEnd::Finished
                } else {
                    LateEnd::NotResumed
                }
            }
            Err(LateEnd::Stalled) => {
                warn!(
                    "FirecrackerAdapter ({vm_id}): The late snapshot made no progress for {}s; \
                     ending Firecracker (pid {})",
                    stall.as_secs(),
                    self.pid
                );
                if self.exit.borrow().is_none() {
                    let _ = kill(Pid::from_raw(self.pid), Signal::SIGKILL);
                }
                LateEnd::Stalled
            }
            Err(end) => {
                info!(
                    "FirecrackerAdapter ({vm_id}): Firecracker ended before the late snapshot \
                     was done"
                );
                end
            }
        };
        // Firecracker writes no more once it answered; once it is ended,
        // when it is gone.
        if end != LateEnd::Finished
            && tokio::time::timeout(KILL_TIMEOUT, self.exit.wait_for(Option::is_some))
                .await
                .is_err()
        {
            warn!(
                "FirecrackerAdapter ({vm_id}): Firecracker (pid {}) outlived SIGKILL",
                self.pid
            );
        }
        discard_snapshot(&vm_id, &self.dir).await;
        end
    }
}

/// Resolves once the file at `memory` has not grown for `stall`, looked at
/// every `poll`: what it takes on disk grows while Firecracker writes it.
async fn stalled(memory: PathBuf, poll: Duration, stall: Duration) {
    let mut last = None;
    let mut since = tokio::time::Instant::now();
    loop {
        tokio::time::sleep(poll).await;
        let path = memory.clone();
        let size = blocking(move || snapshot::disk_usage(&path)).await.ok();
        if size != last {
            last = size;
            since = tokio::time::Instant::now();
        } else if since.elapsed() >= stall {
            return;
        }
    }
}

async fn wait_for_exit(pid: i32, child: Option<Child>) -> Exit {
    if feos_utils::reaper::is_running() && is_child(pid) {
        // The reaper owns waitpid in this process.
        return Some(feos_utils::reaper::wait_for_exit(pid).await);
    }
    if let Some(mut child) = child {
        return tokio::task::spawn_blocking(move || child.wait())
            .await
            .ok()
            .and_then(Result::ok)
            .map(exit_code);
    }
    // Started before this service: not a child it can wait for.
    while process_exists(pid) {
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    None
}

fn exit_code(status: ExitStatus) -> i32 {
    status
        .code()
        .or_else(|| status.signal().map(|signal| 128 + signal))
        .unwrap_or(-1)
}

fn process_exists(pid: i32) -> bool {
    kill(Pid::from_raw(pid), None).is_ok()
}

/// Whether `pid` is a child of this process.
fn is_child(pid: i32) -> bool {
    let Ok(stat) = std::fs::read_to_string(format!("/proc/{pid}/stat")) else {
        return false;
    };
    // "pid (comm) state ppid ...": comm may hold spaces and parentheses.
    stat.rsplit_once(')')
        .and_then(|(_, rest)| rest.split_whitespace().nth(1))
        .and_then(|ppid| ppid.parse::<u32>().ok())
        == Some(std::process::id())
}

fn api_socket(vm_id: &str) -> PathBuf {
    vm_dir(vm_id).join(API_SOCKET)
}

fn read_pid(vm_id: &str) -> Option<i32> {
    std::fs::read_to_string(vm_dir(vm_id).join("firecracker.pid"))
        .ok()?
        .trim()
        .parse()
        .ok()
}

/// The last lines of Firecracker's log from `offset` on.
fn log_tail(path: &Path, offset: u64) -> String {
    let log = std::fs::read(path).unwrap_or_default();
    let log = String::from_utf8_lossy(log.get(offset as usize..).unwrap_or_default());
    let lines: Vec<&str> = log.lines().collect();
    lines[lines.len().saturating_sub(LOG_TAIL_LINES)..].join("\n")
}

/// The Firecracker configuration of a microVM, with the paths Firecracker
/// sees in its jail, and the files to link into the jail. The guest's
/// directory carries the kernel and its initramfs.
fn firecracker_config(
    config: &VmConfig,
    image: &Path,
) -> Result<(Value, Vec<jail::Link>), VmmError> {
    let kernel = image.join("vmlinuz");
    if !kernel.exists() {
        return Err(VmmError::InvalidConfig(format!(
            "{} has no kernel",
            image.display()
        )));
    }
    let mut links = Vec::new();
    let mut link = |source: PathBuf, name: String, disk: bool| {
        let link = jail::Link { source, name, disk };
        let jailed = link.jailed();
        links.push(link);
        jailed
    };
    let mut boot_source = json!({
        "kernel_image_path": link(kernel, "vmlinuz".to_string(), false),
        "boot_args": BOOT_ARGS,
    });
    let initramfs = image.join("initramfs");
    if initramfs.exists() {
        boot_source["initrd_path"] = json!(link(initramfs, "initramfs".to_string(), false));
    }

    let mut interfaces = Vec::new();
    for (index, nic) in config.net.iter().enumerate() {
        let tap = nic
            .tap
            .as_ref()
            .filter(|tap| !tap.tap_name.is_empty())
            .ok_or_else(|| {
                VmmError::InvalidConfig("a network interface needs a TAP device".to_string())
            })?;
        let id = match nic.device_id.as_str() {
            "" => format!("eth{index}"),
            id => id.to_string(),
        };
        let mut interface = json!({"iface_id": id, "host_dev_name": tap.tap_name});
        if !nic.mac_address.is_empty() {
            interface["guest_mac"] = json!(nic.mac_address);
        }
        interfaces.push(interface);
    }

    // A writeback cache, so that the guest's flushes reach the device: a
    // pod's disk is written back to its top in diskd only then.
    let mut drives = Vec::new();
    let mut drive = |id: String, path: &str| {
        drives.push(json!({
            "drive_id": id,
            "path_on_host": link(PathBuf::from(path), id.clone(), true),
            "is_root_device": false,
            "is_read_only": false,
            "cache_type": "Writeback",
        }));
    };
    if let Some(disk) = config
        .data_disk
        .as_ref()
        .filter(|disk| !disk.path.is_empty())
    {
        drive(DATA_DISK.to_string(), &disk.path);
    }
    // Firecracker attaches the drives in this order, and the guest tells
    // the volumes apart by it (ADR 0016).
    for (index, volume) in config.volumes.iter().enumerate() {
        if volume.path.is_empty() {
            return Err(VmmError::InvalidConfig(format!(
                "volume {index} has no disk"
            )));
        }
        drive(format!("{VOLUME_DISK}{index}"), &volume.path);
    }

    let (vcpus, memory_mib) = machine_size(config);
    let machine = json!({"vcpu_count": vcpus, "mem_size_mib": memory_mib});

    let mut fc = json!({
        "boot-source": boot_source,
        "drives": drives,
        "network-interfaces": interfaces,
        "machine-config": machine,
        "vsock": {"guest_cid": GUEST_CID, "uds_path": format!("/{VSOCK}")},
    });
    if let Some(balloon) = config.memory.and_then(|memory| memory.balloon) {
        fc["balloon"] = json!({
            "amount_mib": balloon.amount_mib,
            "deflate_on_oom": balloon.deflate_on_oom,
            "free_page_reporting": balloon.free_page_reporting,
            "stats_polling_interval_s": balloon.stats_interval_s,
        });
    }
    // Version 2 answers a request only with a session token, which takes a
    // PUT: a GET that the guest's code is tricked into sending gets
    // nothing (ADR 0010).
    if !config.metadata_json.is_empty()
        && let Some(interface) = fc["network-interfaces"][0]["iface_id"].as_str()
    {
        fc["mmds-config"] = json!({
            "version": "V2",
            "network_interfaces": [interface],
            "ipv4_address": super::METADATA_ADDRESS.to_string(),
        });
    }
    Ok((fc, links))
}

/// The VM's vCPUs and its memory in MiB.
fn machine_size(config: &VmConfig) -> (u32, u64) {
    let vcpus = config
        .cpus
        .map(|cpus| cpus.vcpu_count)
        .filter(|&n| n > 0)
        .unwrap_or(DEFAULT_VCPUS);
    let memory_mib = config
        .memory
        .map(|memory| memory.size_mib)
        .filter(|&n| n > 0)
        .unwrap_or(DEFAULT_MEMORY_MIB);
    (vcpus, memory_mib)
}

/// The TAP devices of a VM.
fn tap_names(config: &VmConfig) -> Vec<String> {
    config
        .net
        .iter()
        .filter_map(|nic| nic.tap.as_ref().map(|tap| tap.tap_name.clone()))
        .collect()
}

/// Where `name` is: a path, or an executable in $PATH.
fn find_executable(name: &Path) -> PathBuf {
    if name.components().count() > 1 {
        return name.to_path_buf();
    }
    std::env::var_os("PATH")
        .and_then(|path| {
            std::env::split_paths(&path)
                .map(|dir| dir.join(name))
                .find(|candidate| candidate.is_file())
        })
        .unwrap_or_else(|| name.to_path_buf())
}

fn internal(what: &str) -> impl FnOnce(io::Error) -> VmmError + '_ {
    move |e| VmmError::Internal(format!("{what}: {e}"))
}

/// Puts the VM's metadata into the store of its Firecracker, if it has
/// any.
async fn put_metadata(vm_id: &str, api: &Path) -> Result<(), VmmError> {
    let metadata = match std::fs::read(vm_dir(vm_id).join(METADATA)) {
        Ok(metadata) => metadata,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(internal("read the metadata")(e)),
    };
    let metadata: Value = serde_json::from_slice(&metadata)
        .map_err(|e| VmmError::Internal(format!("read the metadata: {e}")))?;
    fc_api::request(api, "PUT", "/mmds", Some(&metadata))
        .await
        .map(drop)
}

async fn firecracker_version(api: &Path) -> Result<String, VmmError> {
    let version = fc_api::request(api, "GET", "/version", None)
        .await?
        .unwrap_or_default();
    Ok(version["firecracker_version"]
        .as_str()
        .unwrap_or_default()
        .to_string())
}

/// A Firecracker process this adapter just started.
struct Spawned {
    pid: i32,
    exit: ExitWatch,
    log: PathBuf,
    log_offset: u64,
}

impl Spawned {
    /// Waits for `ready`. Kills the process if it does not get there in
    /// time, and reports from its log if it ends instead.
    async fn wait(&mut self, ready: impl Future<Output = ()>, what: &str) -> Result<(), VmmError> {
        tokio::select! {
            result = tokio::time::timeout(BOOT_TIMEOUT, ready) => {
                if result.is_err() {
                    let _ = kill(Pid::from_raw(self.pid), Signal::SIGKILL);
                    return Err(VmmError::ProcessSpawnFailed(format!(
                        "Firecracker did not {what} within {}s: {}",
                        BOOT_TIMEOUT.as_secs(),
                        log_tail(&self.log, self.log_offset)
                    )));
                }
            }
            _ = self.exit.wait_for(Option::is_some) => {
                return Err(VmmError::ProcessSpawnFailed(format!(
                    "Firecracker exited: {}",
                    log_tail(&self.log, self.log_offset)
                )));
            }
        }
        Ok(())
    }
}

impl FirecrackerAdapter {
    /// The Firecracker a boot runs: the newest installed, or the one in
    /// $PATH where none is.
    fn vmm_for_boot(&self) -> jail::Vmm {
        match installed::newest_firecracker() {
            Some(dir) => jail::Vmm {
                firecracker: dir.join(crate::FIRECRACKER_BIN),
                jailer: dir.join(crate::JAILER_BIN),
            },
            None => jail::Vmm {
                firecracker: find_executable(&self.firecracker),
                jailer: find_executable(&self.jailer),
            },
        }
    }

    /// Starts Firecracker for the VM. With a configuration file it boots
    /// the VM at once; without one it waits for calls on its API.
    fn spawn(
        &self,
        vm_id: &str,
        vmm: &jail::Vmm,
        config: Option<&str>,
    ) -> Result<Spawned, VmmError> {
        let dir = vm_dir(vm_id);
        jail::prepare(vm_id).map_err(internal("prepare the jail"))?;
        let manifest = jail::Manifest::load(vm_id).map_err(internal("read the jail's manifest"))?;
        jail::prepare_cgroup(vm_id, manifest.cpu_weight)
            .map_err(internal("prepare the VM's cgroup"))?;
        let uid = manifest.uid.to_string();
        // Firecracker binds both sockets and fails if they exist.
        let api = api_socket(vm_id);
        for socket in [&api, &vsock_path(vm_id)] {
            match std::fs::remove_file(socket) {
                Ok(()) => {}
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(e) => return Err(internal("remove a stale socket")(e)),
            }
        }

        // Firecracker's stdin is the console's input, its stdout the
        // console's output. Holding the FIFO open for reading and writing,
        // it never sees an end of input.
        let console_in = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(dir.join("console.in"))
            .map_err(internal("open the console FIFO"))?;
        // A boot starts the console's output over; a restored guest goes on.
        let serial_path = dir.join("serial.log");
        let serial = match config {
            Some(_) => std::fs::File::create(&serial_path),
            None => std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&serial_path),
        }
        .map_err(internal("open the serial log"))?;
        let vmm_log_path = dir.join(VMM_LOG);
        let vmm_log = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&vmm_log_path)
            .map_err(internal("open the VMM log"))?;
        let log_offset = vmm_log.metadata().map(|m| m.len()).unwrap_or(0);
        let stderr = vmm_log.try_clone().map_err(internal("open the VMM log"))?;

        // The jailer becomes Firecracker: the same process, with the same
        // standard streams. It passes the ID on.
        let mut command = Command::new(&vmm.jailer);
        command
            .arg("--id")
            .arg(vm_id)
            .arg("--exec-file")
            .arg(&vmm.firecracker)
            .args(["--uid", &uid, "--gid", &uid])
            .arg("--chroot-base-dir")
            .arg(VM_DIR)
            // With no --cgroup values, the jailer moves into this cgroup
            // and changes nothing else.
            .args(["--cgroup-version", "2"])
            .arg("--parent-cgroup")
            .arg(jail::jailer_cgroup(vm_id))
            .arg("--")
            .args(["--api-sock", &format!("/{API_SOCKET}")]);
        if let Some(config) = config {
            command.arg("--config-file").arg(config);
            // The metadata is in the store before the guest boots.
            if dir.join(METADATA).exists() {
                command.arg("--metadata").arg(format!("/{METADATA}"));
            }
        }
        command
            .args(["--log-path", &format!("/{VMM_LOG}")])
            .stdin(console_in)
            .stdout(serial)
            .stderr(stderr);
        // SAFETY: setsid is async-signal-safe, and so is the closure: nix's
        // conversion of its Errno into an io::Error allocates nothing, where
        // io::Error::other would allocate in the forked child, whose
        // allocator lock another thread may have held. Its own session keeps
        // the VMM away from signals for this process's terminal.
        unsafe {
            command.pre_exec(|| {
                unistd::setsid()?;
                Ok(())
            });
        }
        // A std child: in FeOS the reaper owns waitpid, and a tokio child
        // would race it.
        let child = command
            .spawn()
            .map_err(|e| VmmError::ProcessSpawnFailed(format!("{}: {e}", vmm.jailer.display())))?;
        // The jailer records the pid in the jail.
        let pid = child.id() as i32;
        let exit = self.watch_process(vm_id, pid, Some(child));
        info!("FirecrackerAdapter ({vm_id}): Started Firecracker (pid {pid})");
        Ok(Spawned {
            pid,
            exit,
            log: vmm_log_path,
            log_offset,
        })
    }

    /// Ends a process that did not get the VM running.
    async fn discard(&self, vm_id: &str, vmm: &mut Spawned) {
        self.exits().remove(vm_id);
        let _ = kill(Pid::from_raw(vmm.pid), Signal::SIGKILL);
        let _ = tokio::time::timeout(KILL_TIMEOUT, vmm.exit.wait_for(Option::is_some)).await;
        let _ = std::fs::remove_file(vm_dir(vm_id).join("firecracker.pid"));
    }
}

#[tonic::async_trait]
impl Hypervisor for FirecrackerAdapter {
    async fn create_vm(
        &self,
        vm_id: &str,
        req: CreateVmRequest,
        image_dir: PathBuf,
    ) -> Result<Option<i64>, VmmError> {
        let config = req
            .config
            .ok_or_else(|| VmmError::InvalidConfig("VmConfig is required".to_string()))?;
        let (fc_config, links) = firecracker_config(&config, &image_dir)?;

        let dir = vm_dir(vm_id);
        std::fs::create_dir_all(&dir).map_err(internal("create the VM directory"))?;
        {
            let _jails = self.jails.lock().unwrap_or_else(|e| e.into_inner());
            let (vcpus, _) = machine_size(&config);
            let manifest = jail::Manifest {
                uid: jail::allocate_uid().map_err(internal("allocate a user ID"))?,
                vmm: None,
                cpu_weight: jail::cpu_weight(vcpus),
                links,
                taps: tap_names(&config),
                created_taps: Vec::new(),
            };
            manifest
                .save(vm_id)
                .map_err(internal("write the jail's manifest"))?;
        }
        let fc_config = serde_json::to_vec_pretty(&fc_config)
            .map_err(|e| VmmError::Internal(format!("serialize the configuration: {e}")))?;
        std::fs::write(dir.join(CONFIG), fc_config).map_err(internal("write the configuration"))?;
        let metadata = dir.join(METADATA);
        if config.metadata_json.is_empty() {
            match std::fs::remove_file(&metadata) {
                Ok(()) => {}
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(e) => return Err(internal("remove the old metadata")(e)),
            }
        } else {
            std::fs::write(&metadata, &config.metadata_json)
                .map_err(internal("write the metadata"))?;
        }
        match unistd::mkfifo(&dir.join("console.in"), Mode::S_IRUSR | Mode::S_IWUSR) {
            Ok(()) | Err(nix::Error::EEXIST) => {}
            Err(e) => return Err(VmmError::Internal(format!("create the console FIFO: {e}"))),
        }
        info!(
            "FirecrackerAdapter ({vm_id}): Configuration written to {}",
            dir.display()
        );
        // Firecracker starts with the boot.
        Ok(None)
    }

    async fn start_vm(&self, req: StartVmRequest) -> Result<i64, VmmError> {
        let vm_id = req.vm_id.as_str();
        if !vm_dir(vm_id).join(CONFIG).exists() {
            return Err(VmmError::VmNotFound(vm_id.to_string()));
        }
        // A cold boot: the memory in a snapshot is gone for good.
        let id = vm_id.to_string();
        blocking(move || snapshot::remove(&id))
            .await
            .map_err(internal("remove the snapshot"))?;

        // The newest Firecracker, which a snapshot of this boot needs again.
        let booted = self.vmm_for_boot();
        let mut manifest =
            jail::Manifest::load(vm_id).map_err(internal("read the jail's manifest"))?;
        manifest.vmm = Some(booted.clone());
        manifest
            .save(vm_id)
            .map_err(internal("write the jail's manifest"))?;
        info!(
            "FirecrackerAdapter ({vm_id}): Booting with {}",
            booted.firecracker.display()
        );
        let mut vmm = self.spawn(vm_id, &booted, Some(&format!("/{CONFIG}")))?;
        let api = api_socket(vm_id);
        let booted = async {
            loop {
                if let Ok(Some(info)) = fc_api::request(&api, "GET", "/", None).await
                    && info["state"] == "Running"
                {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        };
        vmm.wait(booted, "boot the VM").await?;
        Ok(vmm.pid.into())
    }

    async fn suspend_vm(&self, vm_id: &str, settle_free_pages: bool) -> Result<u64, VmmError> {
        self.check_free(vm_id)?;
        let pid = read_pid(vm_id)
            .filter(|&pid| self.is_vmm_process(vm_id, pid.into()))
            .ok_or_else(|| VmmError::VmNotFound(vm_id.to_string()))?;
        let mut exit = self.exit_watch(vm_id)?;
        let api = api_socket(vm_id);
        if settle_free_pages {
            snapshot::settle_free_pages(pid).await;
        }

        let state = fc_api::request(&api, "GET", "/", None)
            .await?
            .unwrap_or_default();
        let was_running = state["state"] == "Running";
        if was_running {
            let body = json!({"state": "Paused"});
            fc_api::request(&api, "PATCH", "/vm", Some(&body)).await?;
        }
        let prepared = {
            let id = vm_id.to_string();
            tokio::task::spawn_blocking(move || {
                snapshot::remove(&id).map_err(internal("remove the old snapshot"))?;
                std::fs::create_dir_all(snapshot::dir(&id))
                    .map_err(internal("create the snapshot directory"))?;
                jail::give(&id, &snapshot::dir(&id))
                    .map_err(internal("hand the snapshot directory to the VM"))
            })
            .await
            .unwrap_or_else(|e| {
                Err(VmmError::Internal(format!(
                    "ready the snapshot directory: {e}"
                )))
            })
        };
        let answered = match prepared {
            Ok(()) => write_snapshot(&api, SNAPSHOT_TIMEOUT).await,
            Err(e) => Written::Answered(Err(e)),
        };
        let written = match answered {
            Written::Answered(answer) => answer,
            Written::Late(create) => {
                // The snapshot's files stay alone while Firecracker writes
                // them, and the VM takes no other change.
                let late = LateSnapshot {
                    vm_id: vm_id.to_string(),
                    pid,
                    api: api.clone(),
                    dir: snapshot::dir(vm_id),
                    create,
                    exit,
                    resume: was_running,
                };
                let mark = self.mark_late(vm_id);
                tokio::spawn(async move {
                    let _mark = mark;
                    late.wait_out(LATE_POLL, LATE_STALL).await;
                });
                let message = format!(
                    "the snapshot was not written within {}s: Firecracker writes it on, and the \
                     VM {} once it is done",
                    SNAPSHOT_TIMEOUT.as_secs(),
                    if was_running {
                        "resumes"
                    } else {
                        "stays paused"
                    }
                );
                warn!("FirecrackerAdapter ({vm_id}): Suspend failed: {message}");
                return Err(VmmError::Busy(message));
            }
        };
        let written = match written {
            Ok(()) => {
                async {
                    snapshot::Host::this(&firecracker_version(&api).await?)
                        .save(vm_id)
                        .map_err(internal("record the host"))
                }
                .await
            }
            Err(e) => Err(e),
        };
        if let Err(e) = written {
            // Firecracker answered: it writes the snapshot no more.
            discard_snapshot(vm_id, &snapshot::dir(vm_id)).await;
            if was_running {
                resume_or_end(vm_id, pid, &api, &exit).await;
            }
            return Err(e);
        }

        // An end, not a crash: the health check of this process must not
        // report it.
        self.exits().remove(vm_id);
        let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
        if tokio::time::timeout(KILL_TIMEOUT, exit.wait_for(Option::is_some))
            .await
            .is_err()
        {
            warn!("FirecrackerAdapter ({vm_id}): Firecracker (pid {pid}) outlived SIGKILL");
        }
        let _ = std::fs::remove_file(vm_dir(vm_id).join("firecracker.pid"));

        let memory = snapshot::memory(vm_id);
        let punched = {
            let memory = memory.clone();
            tokio::task::spawn_blocking(move || snapshot::punch_zero_pages(&memory)).await
        };
        match punched {
            Ok(Ok(bytes)) => info!(
                "FirecrackerAdapter ({vm_id}): Suspended; {} MiB of zeroes punched out of the memory",
                bytes >> 20
            ),
            Ok(Err(e)) => warn!("FirecrackerAdapter ({vm_id}): Failed to punch holes: {e}"),
            Err(e) => warn!("FirecrackerAdapter ({vm_id}): Failed to punch holes: {e}"),
        }
        snapshot::disk_usage(&memory).map_err(internal("measure the snapshot"))
    }

    async fn restore_vm(
        &self,
        vm_id: &str,
        remote: Option<snapshot::Remote>,
    ) -> Result<i64, VmmError> {
        // A local snapshot's memory is a file; a remote one's is served
        // on a socket in the VM's directory.
        let memory_backend = match &remote {
            None if !snapshot::exists(vm_id) => {
                return Err(VmmError::SnapshotUnusable(
                    "the VM has no snapshot".to_string(),
                ));
            }
            None => json!({"backend_type": "File", "backend_path": snapshot::JAILED_MEMORY}),
            Some(remote) => {
                snapshot::receive(vm_id, remote).map_err(internal("take the snapshot in"))?;
                json!({"backend_type": "Uffd", "backend_path": format!("/{}", snapshot::MEMORY_SOCKET)})
            }
        };
        let saved = snapshot::Host::load(vm_id)
            .map_err(|e| VmmError::SnapshotUnusable(format!("host record: {e}")))?;

        // The Firecracker that wrote the snapshot: the one this VM booted
        // with, or for a remote one the installed one of its version,
        // which the VM keeps from now on.
        let recorded = match &remote {
            None => jail::Manifest::load(vm_id)
                .map_err(internal("read the jail's manifest"))?
                .vmm
                .unwrap_or_else(|| self.vmm_for_boot()),
            Some(_) => {
                let dir = installed::firecracker(&saved.firecracker_version).ok_or_else(|| {
                    VmmError::SnapshotUnusable(format!(
                        "the snapshot is from Firecracker {:?}, which is not installed",
                        saved.firecracker_version
                    ))
                })?;
                let vmm = jail::Vmm {
                    firecracker: dir.join(crate::FIRECRACKER_BIN),
                    jailer: dir.join(crate::JAILER_BIN),
                };
                let mut manifest =
                    jail::Manifest::load(vm_id).map_err(internal("read the jail's manifest"))?;
                manifest.vmm = Some(vmm.clone());
                manifest
                    .save(vm_id)
                    .map_err(internal("write the jail's manifest"))?;
                vmm
            }
        };
        for binary in [&recorded.firecracker, &recorded.jailer] {
            if !binary.is_file() {
                return Err(VmmError::SnapshotUnusable(format!(
                    "{} wrote the snapshot and is not installed any more",
                    binary.display()
                )));
            }
        }
        let mut vmm = self.spawn(vm_id, &recorded, None)?;
        let api = api_socket(vm_id);
        let listening = async {
            while fc_api::request(&api, "GET", "/", None).await.is_err() {
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        };
        vmm.wait(listening, "answer on its API").await?;

        let loaded = async {
            let host = snapshot::Host::this(&firecracker_version(&api).await?);
            if let Some(why) = saved.mismatch(&host) {
                return Err(VmmError::SnapshotUnusable(why));
            }
            // The vCPUs start only once the load worked, so that a failure
            // up to here leaves the snapshot good.
            let body = json!({
                "snapshot_path": snapshot::JAILED_VMSTATE,
                "mem_backend": memory_backend,
                "resume_vm": false,
            });
            fc_api::request_within(&api, "PUT", "/snapshot/load", Some(&body), SNAPSHOT_TIMEOUT)
                .await
                .map(drop)
        }
        .await;
        if let Err(e) = loaded {
            self.discard(vm_id, &mut vmm).await;
            return Err(e);
        }
        // The snapshot keeps where the metadata is served, not what: it goes
        // in again before the guest runs, as the VM was made, with the
        // addresses it has on this node. Without it the guest runs on.
        if let Err(e) = put_metadata(vm_id, &api).await {
            warn!("FirecrackerAdapter ({vm_id}): The guest runs without its metadata: {e}");
        }

        let body = json!({"state": "Resumed"});
        if let Err(e) = fc_api::request(&api, "PATCH", "/vm", Some(&body)).await {
            let state = fc_api::request(&api, "GET", "/", None).await.ok().flatten();
            if state.is_some_and(|state| state["state"] == "Paused") {
                self.discard(vm_id, &mut vmm).await;
                return Err(e);
            }
            warn!(
                "FirecrackerAdapter ({vm_id}): The resume reported {e}, but the VM is not paused"
            );
        }
        // The guest ran: its disks moved on from the snapshot. Firecracker
        // keeps the memory file mapped; its blocks go when it exits.
        let id = vm_id.to_string();
        if let Err(e) = blocking(move || snapshot::remove(&id)).await {
            warn!("FirecrackerAdapter ({vm_id}): Failed to remove the restored snapshot: {e}");
        }
        info!("FirecrackerAdapter ({vm_id}): Restored from the snapshot");
        Ok(vmm.pid.into())
    }

    async fn healthcheck_vm(
        &self,
        vm_id: String,
        broadcast_tx: mpsc::Sender<VmEventWrapper>,
        mut cancel_bus: broadcast::Receiver<PodId>,
    ) {
        let Ok(parsed_id) = PodId::parse(&vm_id) else {
            warn!("FirecrackerAdapter ({vm_id}): Not a pod ID; not watching it");
            return;
        };
        let mut exit = match self.exit_watch(&vm_id) {
            Ok(exit) => exit,
            Err(e) => {
                warn!("FirecrackerAdapter ({vm_id}): Cannot watch the VM: {e}");
                return;
            }
        };
        info!("FirecrackerAdapter ({vm_id}): Watching the VMM process.");

        let ended = loop {
            tokio::select! {
                ended = exit.wait_for(Option::is_some) => {
                    // A sender only goes away after it sent.
                    break ended.ok().and_then(|ended| *ended).flatten();
                }
                cancelled = cancel_bus.recv() => match cancelled {
                    Ok(id) if id == parsed_id => {
                        info!("FirecrackerAdapter ({vm_id}): Stopped watching.");
                        return;
                    }
                    Ok(_) | Err(broadcast::error::RecvError::Lagged(_)) => {}
                    Err(broadcast::error::RecvError::Closed) => return,
                },
            }
        };
        // A later boot may have replaced this process already, and its
        // Running must not be overwritten by this process's end.
        let current = {
            let mut exits = self.exits();
            let current = exits.get(&vm_id).is_some_and(|rx| rx.same_channel(&exit));
            if current {
                exits.remove(&vm_id);
            }
            current
        };
        if !current {
            info!("FirecrackerAdapter ({vm_id}): A previous VMM process ended.");
            return;
        }

        let (state, reason) = match ended {
            Some(0) => (VmState::Stopped, "The guest shut down".to_string()),
            Some(code) => (
                VmState::Crashed,
                format!("Firecracker exited with code {code}"),
            ),
            None => (VmState::Crashed, "Firecracker exited".to_string()),
        };
        info!("FirecrackerAdapter ({vm_id}): {reason}");
        super::broadcast_state_change_event(
            &broadcast_tx,
            &vm_id,
            "vm-health-monitor",
            VmStateChangedEvent {
                new_state: state as i32,
                reason,
            },
            None,
        )
        .await;
    }

    fn check_free(&self, vm_id: &str) -> Result<(), VmmError> {
        if lock(&self.late).contains(vm_id) {
            return Err(VmmError::Busy(format!(
                "Firecracker still writes the snapshot that the last suspend of VM {vm_id} gave \
                 up on; the VM is as that suspend found it once it is done"
            )));
        }
        Ok(())
    }

    fn is_vmm_process(&self, vm_id: &str, pid: i64) -> bool {
        // A pid from a record may belong to another process by now. The
        // jailer and the Firecracker it becomes carry the VM's ID.
        let Ok(cmdline) = std::fs::read(format!("/proc/{pid}/cmdline")) else {
            return false;
        };
        let args: Vec<&[u8]> = cmdline.split(|&b| b == 0).collect();
        args.windows(2)
            .any(|pair| pair[0] == b"--id" && pair[1] == vm_id.as_bytes())
    }

    async fn delete_vm(
        &self,
        req: DeleteVmRequest,
        process_id: Option<i64>,
    ) -> Result<DeleteVmResponse, VmmError> {
        let vm_id = req.vm_id.as_str();
        let pid = process_id.or_else(|| read_pid(vm_id).map(i64::from));
        if let Some(pid) = pid
            && self.is_vmm_process(vm_id, pid)
        {
            info!("FirecrackerAdapter ({vm_id}): Killing Firecracker (pid {pid})");
            let exit = self.exit_watch(vm_id).ok();
            let _ = kill(Pid::from_raw(pid as i32), Signal::SIGKILL);
            // Its files go only once it is gone.
            if let Some(mut exit) = exit
                && tokio::time::timeout(KILL_TIMEOUT, exit.wait_for(Option::is_some))
                    .await
                    .is_err()
            {
                warn!("FirecrackerAdapter ({vm_id}): Firecracker (pid {pid}) outlived SIGKILL");
            }
        }
        self.exits().remove(vm_id);

        let id = vm_id.to_string();
        let removed = blocking(move || {
            jail::release(&id);
            std::fs::remove_dir_all(jail_dir(&id))
        })
        .await;
        match removed {
            Ok(()) => info!("FirecrackerAdapter ({vm_id}): Removed the VM directory"),
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => warn!("FirecrackerAdapter ({vm_id}): Failed to remove the VM directory: {e}"),
        }
        Ok(DeleteVmResponse {})
    }

    async fn open_console(&self, vm_id: &str) -> Result<DuplexStream, VmmError> {
        let dir = vm_dir(vm_id);
        let mut output = tokio::fs::File::open(dir.join("serial.log"))
            .await
            .map_err(|_| VmmError::VmNotFound(vm_id.to_string()))?;
        let mut position = output
            .seek(SeekFrom::End(0))
            .await
            .map_err(internal("read the serial log"))?;
        let mut input = tokio::net::unix::pipe::OpenOptions::new()
            .read_write(true)
            .open_sender(dir.join("console.in"))
            .map_err(internal("open the console FIFO"))?;

        let (ours, theirs) = tokio::io::duplex(64 * 1024);
        tokio::spawn(async move {
            let (mut from_client, mut to_client) = tokio::io::split(ours);
            let mut keys = [0u8; 1024];
            let mut text = vec![0u8; 16 * 1024];
            let mut poll = tokio::time::interval(CONSOLE_POLL);
            loop {
                tokio::select! {
                    read = from_client.read(&mut keys) => match read {
                        Ok(0) | Err(_) => return,
                        Ok(n) => {
                            if input.write_all(&keys[..n]).await.is_err() {
                                return;
                            }
                        }
                    },
                    _ = poll.tick() => {
                        // A new boot starts the log over.
                        if output.metadata().await.is_ok_and(|m| m.len() < position) {
                            position = output.seek(SeekFrom::Start(0)).await.unwrap_or(0);
                        }
                        loop {
                            match output.read(&mut text).await {
                                Ok(0) => break,
                                Ok(n) => {
                                    position += n as u64;
                                    if to_client.write_all(&text[..n]).await.is_err() {
                                        return;
                                    }
                                }
                                Err(_) => return,
                            }
                        }
                    }
                }
            }
        });
        Ok(theirs)
    }

    async fn ping_vm(&self, req: PingVmRequest) -> Result<PingVmResponse, VmmError> {
        let info = fc_api::request(&api_socket(&req.vm_id), "GET", "/", None)
            .await?
            .unwrap_or_default();
        let field = |name: &str| info[name].as_str().unwrap_or_default().to_string();
        Ok(PingVmResponse {
            build_version: field("app_name"),
            version: field("vmm_version"),
            pid: read_pid(&req.vm_id).unwrap_or(0).into(),
            features: Vec::new(),
        })
    }

    async fn shutdown_vm(&self, req: ShutdownVmRequest) -> Result<ShutdownVmResponse, VmmError> {
        let vm_id = req.vm_id.as_str();
        self.check_free(vm_id)?;
        let mut exit = self.exit_watch(vm_id)?;
        // An end that was asked for, which the caller reports: the health
        // check must not report it too. It watches again if the guest
        // stays.
        let watched = self.exits().remove(vm_id);
        let deadline = tokio::time::Instant::now() + SHUTDOWN_TIMEOUT;
        let keys = json!({"action_type": "SendCtrlAltDel"});
        let ended = async {
            loop {
                // A guest that is still booting has no keyboard driver yet
                // and misses the keys: they are sent until it goes.
                let sent =
                    fc_api::request(&api_socket(vm_id), "PUT", "/actions", Some(&keys)).await;
                if exit.borrow().is_some() {
                    return Ok(());
                }
                sent?;
                let wait = CTRL_ALT_DEL_REPEAT
                    .min(deadline.saturating_duration_since(tokio::time::Instant::now()));
                if tokio::time::timeout(wait, exit.wait_for(Option::is_some))
                    .await
                    .is_ok()
                {
                    return Ok(());
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err(VmmError::ApiOperationFailed(format!(
                        "the guest did not shut down within {}s of Ctrl-Alt-Del",
                        SHUTDOWN_TIMEOUT.as_secs()
                    )));
                }
            }
        }
        .await;
        if let (Err(_), Some(watched)) = (&ended, watched) {
            self.exits().insert(vm_id.to_string(), watched);
        }
        ended.map(|()| ShutdownVmResponse {})
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::microvm::{
        BalloonConfig, CpuConfig, DataDiskConfig, MemoryConfig, NetConfig, TapConfig,
    };

    fn image(files: &[&str]) -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        for file in files {
            std::fs::write(dir.path().join(file), b"").unwrap();
        }
        dir
    }

    #[test]
    fn an_image_without_a_kernel_is_refused() {
        let image = image(&["initramfs"]);
        let config = VmConfig::default();
        assert!(firecracker_config(&config, image.path()).is_err());
    }

    #[test]
    fn a_kernel_and_an_initramfs_boot_with_defaults() {
        let image = image(&["vmlinuz", "initramfs"]);
        let (fc, links) = firecracker_config(&VmConfig::default(), image.path()).unwrap();
        assert_eq!(fc["boot-source"]["boot_args"], BOOT_ARGS);
        assert_eq!(fc["boot-source"]["kernel_image_path"], "/vmlinuz");
        assert_eq!(fc["boot-source"]["initrd_path"], "/initramfs");
        assert_eq!(links.len(), 2);
        assert_eq!(links[1].source, image.path().join("initramfs"));
        assert!(!links[1].disk);
        assert_eq!(fc["drives"], json!([]));
        assert_eq!(
            fc["machine-config"],
            json!({"vcpu_count": 1, "mem_size_mib": 512})
        );
        assert_eq!(fc["vsock"]["guest_cid"], 3);
        assert_eq!(fc["vsock"]["uds_path"], "/v.sock");
    }

    #[test]
    fn the_size_and_the_network_are_configured() {
        let image = image(&["vmlinuz", "initramfs"]);
        let config = VmConfig {
            cpus: Some(CpuConfig { vcpu_count: 2 }),
            memory: Some(MemoryConfig {
                size_mib: 1024,
                balloon: None,
            }),
            net: vec![NetConfig {
                tap: Some(TapConfig {
                    tap_name: "tap0".to_string(),
                }),
                mac_address: "02:00:00:00:00:01".to_string(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let (fc, _) = firecracker_config(&config, image.path()).unwrap();
        assert_eq!(fc["drives"], json!([]));
        assert_eq!(tap_names(&config), vec!["tap0".to_string()]);
        assert_eq!(
            fc["network-interfaces"],
            json!([{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "02:00:00:00:00:01"}])
        );
        assert_eq!(
            fc["machine-config"],
            json!({"vcpu_count": 2, "mem_size_mib": 1024})
        );
        assert_eq!(fc.get("mmds-config"), None);
    }

    #[test]
    fn metadata_is_served_on_the_first_interface_in_version_2() {
        let image = image(&["vmlinuz"]);
        let nic = |tap: &str, id: &str| NetConfig {
            device_id: id.to_string(),
            tap: Some(TapConfig {
                tap_name: tap.to_string(),
            }),
            ..Default::default()
        };
        let config = VmConfig {
            net: vec![nic("tap0", "pod"), nic("tap1", "")],
            metadata_json: r#"{"feos": {"pod-id": "x"}}"#.to_string(),
            ..Default::default()
        };
        let (fc, _) = firecracker_config(&config, image.path()).unwrap();
        assert_eq!(
            fc["mmds-config"],
            json!({"version": "V2", "network_interfaces": ["pod"], "ipv4_address": "169.254.169.254"})
        );
    }

    #[test]
    fn a_balloon_is_configured_at_boot() {
        let image = image(&["vmlinuz"]);
        let config = VmConfig {
            memory: Some(MemoryConfig {
                size_mib: 256,
                balloon: Some(BalloonConfig {
                    amount_mib: 16,
                    deflate_on_oom: true,
                    free_page_reporting: true,
                    stats_interval_s: 1,
                }),
            }),
            ..Default::default()
        };
        let (fc, _) = firecracker_config(&config, image.path()).unwrap();
        assert_eq!(
            fc["balloon"],
            json!({"amount_mib": 16, "deflate_on_oom": true, "free_page_reporting": true, "stats_polling_interval_s": 1})
        );
        let (fc, _) = firecracker_config(&VmConfig::default(), image.path()).unwrap();
        assert!(fc.get("balloon").is_none());
    }

    #[test]
    fn the_data_disk_is_linked_in_with_a_writeback_cache() {
        let image = image(&["vmlinuz", "initramfs"]);
        let config = VmConfig {
            data_disk: Some(DataDiskConfig {
                path: "/var/lib/feos/disk/live/pod/disk".to_string(),
            }),
            ..Default::default()
        };
        let (fc, links) = firecracker_config(&config, image.path()).unwrap();
        assert_eq!(
            fc["drives"],
            json!([{"drive_id": "data", "path_on_host": "/data", "is_root_device": false,
                    "is_read_only": false, "cache_type": "Writeback"}])
        );
        let disk = links.last().unwrap();
        assert_eq!(
            disk.source,
            PathBuf::from("/var/lib/feos/disk/live/pod/disk")
        );
        assert!(disk.disk && disk.name == "data");
    }

    #[test]
    fn volumes_follow_the_data_disk_in_their_order() {
        let image = image(&["vmlinuz", "initramfs"]);
        let disk = |path: &str| DataDiskConfig {
            path: path.to_string(),
        };
        let config = VmConfig {
            data_disk: Some(disk("/var/lib/feos/disk/live/pod/disk")),
            volumes: vec![
                disk("/var/lib/feos/disk/live/volume-a/disk"),
                disk("/var/lib/feos/disk/live/volume-b/disk"),
            ],
            ..Default::default()
        };
        let (fc, links) = firecracker_config(&config, image.path()).unwrap();
        let drives: Vec<_> = fc["drives"]
            .as_array()
            .unwrap()
            .iter()
            .map(|drive| {
                (
                    drive["drive_id"].as_str().unwrap(),
                    drive["path_on_host"].as_str().unwrap(),
                )
            })
            .collect();
        assert_eq!(
            drives,
            [
                ("data", "/data"),
                ("volume0", "/volume0"),
                ("volume1", "/volume1")
            ]
        );
        assert!(fc["drives"]
            .as_array()
            .unwrap()
            .iter()
            .all(|drive| drive["cache_type"] == "Writeback" && drive["is_read_only"] == false));
        let volume = links.last().unwrap();
        assert_eq!(
            volume.source,
            PathBuf::from("/var/lib/feos/disk/live/volume-b/disk")
        );
        assert!(volume.disk && volume.name == "volume1");

        let config = VmConfig {
            volumes: vec![disk("")],
            ..Default::default()
        };
        assert!(firecracker_config(&config, image.path()).is_err());
    }

    #[test]
    fn a_nic_without_a_tap_is_refused() {
        let image = image(&["vmlinuz"]);
        let config = VmConfig {
            net: vec![NetConfig::default()],
            ..Default::default()
        };
        assert!(firecracker_config(&config, image.path()).is_err());
    }

    #[test]
    fn executables_are_found_in_the_path() {
        let sh = find_executable(Path::new("sh"));
        assert!(sh.is_absolute() && sh.ends_with("sh"));
        assert_eq!(
            find_executable(Path::new("/opt/firecracker")),
            PathBuf::from("/opt/firecracker")
        );
    }

    #[test]
    fn this_process_is_not_its_own_child() {
        assert!(!is_child(std::process::id() as i32));
        let mut child = Command::new("sleep").arg("5").spawn().unwrap();
        assert!(is_child(child.id() as i32));
        child.kill().unwrap();
        child.wait().unwrap();
    }

    /// A Firecracker API on `dir/fc.sock` that answers every request 204,
    /// `PUT /snapshot/create` only after `create`, or never, and records
    /// each as "METHOD PATH BODY" once it answers it.
    fn fake_api(dir: &Path, create: Option<Duration>) -> (PathBuf, Arc<Mutex<Vec<String>>>) {
        let path = dir.join("fc.sock");
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        let answered = Arc::new(Mutex::new(Vec::new()));
        let log = answered.clone();
        tokio::spawn(async move {
            while let Ok((mut conn, _)) = listener.accept().await {
                let log = log.clone();
                tokio::spawn(async move {
                    let mut text = String::new();
                    let mut chunk = [0u8; 1024];
                    let (head, body) = loop {
                        let n = conn.read(&mut chunk).await.unwrap_or(0);
                        if n == 0 {
                            return;
                        }
                        text.push_str(&String::from_utf8_lossy(&chunk[..n]));
                        if let Some((head, body)) = text.split_once("\r\n\r\n") {
                            let length = head
                                .lines()
                                .find_map(|line| line.strip_prefix("Content-Length: "))
                                .and_then(|length| length.trim().parse::<usize>().ok())
                                .unwrap_or(0);
                            if body.len() >= length {
                                break (head.to_string(), body.to_string());
                            }
                        }
                    };
                    let request: Vec<&str> = head.split(' ').take(2).collect();
                    let request = request.join(" ");
                    if request == "PUT /snapshot/create" {
                        match create {
                            Some(after) => tokio::time::sleep(after).await,
                            None => std::future::pending().await,
                        }
                    }
                    lock(&log).push(format!("{request} {body}"));
                    let _ = conn
                        .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                        .await;
                    tokio::time::sleep(Duration::from_secs(60)).await;
                });
            }
        });
        (path, answered)
    }

    /// A snapshot's directory in `dir`, with a memory file.
    fn snapshot_dir(dir: &Path) -> PathBuf {
        let snapshot = dir.join("snapshot");
        std::fs::create_dir(&snapshot).unwrap();
        std::fs::write(snapshot.join("memory"), [1u8; 8192]).unwrap();
        snapshot
    }

    /// A process that stands in for Firecracker, and the watch of its exit.
    fn vmm_process() -> (i32, ExitWatch) {
        let child = Command::new("sleep").arg("60").spawn().unwrap();
        let pid = child.id() as i32;
        let (tx, exit) = watch::channel(None);
        tokio::spawn(async move {
            let _ = tx.send(Some(wait_for_exit(pid, Some(child)).await));
        });
        (pid, exit)
    }

    #[tokio::test]
    async fn a_late_snapshot_is_left_alone_until_firecracker_is_done_then_the_vm_resumes() {
        let dir = tempfile::tempdir().unwrap();
        let (api, answered) = fake_api(dir.path(), Some(Duration::from_millis(800)));
        let snapshot = snapshot_dir(dir.path());
        let (pid, exit) = vmm_process();
        let Written::Late(create) = write_snapshot(&api, Duration::from_millis(100)).await else {
            panic!("the snapshot was answered within its deadline");
        };
        // Past the deadline, Firecracker still writes: nothing touches the
        // snapshot, and the VM is not resumed under it.
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert!(snapshot.join("memory").exists());
        assert!(lock(&answered).is_empty());

        let late = LateSnapshot {
            vm_id: "vm".into(),
            pid,
            api,
            dir: snapshot.clone(),
            create,
            exit: exit.clone(),
            resume: true,
        };
        let end = late
            .wait_out(Duration::from_millis(50), Duration::from_secs(30))
            .await;
        assert_eq!(end, LateEnd::Finished);
        // Resumed once Firecracker answered, and the snapshot discarded.
        let answered = lock(&answered).clone();
        assert_eq!(answered.len(), 2, "{answered:?}");
        assert!(
            answered[0].starts_with("PUT /snapshot/create {"),
            "{answered:?}"
        );
        assert_eq!(answered[1], r#"PATCH /vm {"state":"Resumed"}"#);
        assert!(!snapshot.exists());
        // Firecracker runs on.
        assert_eq!(*exit.borrow(), None);
        let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
    }

    #[tokio::test]
    async fn a_late_snapshot_that_makes_no_progress_ends_firecracker() {
        let dir = tempfile::tempdir().unwrap();
        let (api, answered) = fake_api(dir.path(), None);
        let snapshot = snapshot_dir(dir.path());
        let (pid, exit) = vmm_process();
        let Written::Late(create) = write_snapshot(&api, Duration::from_millis(50)).await else {
            panic!("the snapshot was answered");
        };
        let late = LateSnapshot {
            vm_id: "vm".into(),
            pid,
            api,
            dir: snapshot.clone(),
            create,
            exit: exit.clone(),
            resume: true,
        };
        let end = late
            .wait_out(Duration::from_millis(50), Duration::from_millis(300))
            .await;
        assert_eq!(end, LateEnd::Stalled);
        // Ended, never resumed, and the snapshot gone once it was.
        assert_eq!(*exit.borrow(), Some(Some(128 + Signal::SIGKILL as i32)));
        assert!(lock(&answered).is_empty());
        assert!(!snapshot.exists());
    }

    #[tokio::test]
    async fn a_vm_with_a_late_snapshot_refuses_changes_until_it_is_done() {
        let adapter = FirecrackerAdapter::new("firecracker".into(), "jailer".into());
        let mark = adapter.mark_late("vm");
        for refused in [
            adapter.check_free("vm").err(),
            adapter.suspend_vm("vm", false).await.err(),
            adapter
                .shutdown_vm(ShutdownVmRequest { vm_id: "vm".into() })
                .await
                .err(),
        ] {
            let refused = refused.expect("refused while the snapshot is late");
            assert!(matches!(refused, VmmError::Busy(_)), "{refused}");
            assert_eq!(
                tonic::Status::from(refused).code(),
                tonic::Code::Unavailable
            );
        }
        assert!(adapter.check_free("other").is_ok());
        drop(mark);
        assert!(adapter.check_free("vm").is_ok());
    }
}
