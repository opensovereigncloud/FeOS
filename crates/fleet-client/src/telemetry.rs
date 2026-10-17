// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's telemetry, the heartbeat (fleet.proto `Telemetry`): what it
//! has and how busy it is. What is cheap comes from /proc and statvfs at
//! every report; what changes rarely, the installed versions and diskd's
//! object store, once a minute.

use crate::token::sha256_hex;
use feos_proto::fleet_service::{Telemetry, Versions};
use feos_proto::pod_service::{PodInfo, PodState};
use log::warn;
use std::future::Future;
use std::time::Duration;
use tokio::time::{timeout, Instant};

/// Of the file system that holds the pods' disks: diskd's state directory,
/// or feosd's.
const DISK_DIRS: [&str; 2] = ["/var/lib/feos/disk", "/var/lib/feos"];
/// How long the installed versions and diskd's answer are kept.
const SLOW: Duration = Duration::from_secs(60);
const DISKD_TIMEOUT: Duration = Duration::from_secs(1);

/// What feosd knows of the node that the control plane is told.
pub trait Inventory: Send + Sync + 'static {
    /// The node's pods.
    fn pods(&self) -> Vec<PodInfo>;
    /// The Firecracker and guest versions installed side by side.
    fn installed(&self) -> Installed;
    /// diskd's object store and version, or None when diskd does not
    /// answer.
    fn diskd(&self) -> impl Future<Output = Option<Diskd>> + Send;
    /// The node's prefix for pods, such as "fd5e:1b2c:93a0:7::/64", once
    /// it has one; the configured one is reported until then.
    fn pod_prefix(&self) -> Option<String> {
        None
    }
    /// The node's WireGuard key and port, if it is on the cluster's mesh
    /// (ADR 0017). A node without, which has a prefix of its network's,
    /// asks only whether the cluster is meshed.
    fn mesh_key(&self) -> Option<([u8; 32], u16)> {
        None
    }
    /// Takes what the control plane answered to the node's JoinMesh: the
    /// cluster's ULA and the node's /64 of it, both empty in a cluster
    /// whose network is routed. An answer that is not taken is asked for
    /// again.
    fn joined_mesh(
        &self,
        pod_network: String,
        pod_prefix: String,
    ) -> impl Future<Output = Result<(), String>> + Send {
        let _ = (pod_network, pod_prefix);
        async { Ok(()) }
    }
}

/// The versions installed side by side (ADR 0009), newest first.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Installed {
    pub firecracker: Vec<String>,
    pub guest: Vec<String>,
}

/// What diskd says of itself.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Diskd {
    /// Whether it has an object store, without which no pod hibernates.
    pub object_store: bool,
    /// Which one: fleet.proto's `object_store_id`.
    pub object_store_id: String,
    pub version: String,
}

pub(crate) struct Collector<I> {
    inventory: I,
    /// What stays as long as feosd runs.
    fixed: Telemetry,
    slow: Option<(Instant, Installed, Diskd)>,
}

impl<I: Inventory> Collector<I> {
    pub(crate) fn new(inventory: I, start_id: &str, feosd: &str, pod_prefix: &str) -> Self {
        let (cpu_model, cpu_cores) = std::fs::read_to_string("/proc/cpuinfo")
            .map(|text| cpuinfo(&text))
            .unwrap_or_default();
        let kernel = nix::sys::utsname::uname()
            .map(|uname| uname.release().to_string_lossy().into_owned())
            .unwrap_or_default();
        let fixed = Telemetry {
            start_id: start_id.to_string(),
            arch: std::env::consts::ARCH.to_string(),
            cpu_model,
            cpu_cores,
            pod_prefix: pod_prefix.to_string(),
            versions: Some(Versions {
                feosd: feosd.to_string(),
                kernel,
                ..Default::default()
            }),
            ..Default::default()
        };
        Self {
            inventory,
            fixed,
            slow: None,
        }
    }

    pub(crate) fn inventory(&self) -> &I {
        &self.inventory
    }

    pub(crate) async fn collect(&mut self) -> Telemetry {
        let mut report = self.fixed.clone();
        if let Some(prefix) = self.inventory.pod_prefix() {
            report.pod_prefix = prefix;
        }
        report.hostname = nix::unistd::gethostname()
            .map(|name| name.to_string_lossy().into_owned())
            .unwrap_or_default();
        if let Ok(text) = std::fs::read_to_string("/proc/loadavg") {
            report.load1 = loadavg(&text);
        }
        if let Ok(text) = std::fs::read_to_string("/proc/meminfo") {
            (report.memory_total_bytes, report.memory_available_bytes) = meminfo(&text);
        }
        (report.disk_total_bytes, report.disk_available_bytes) = disk();

        let pods = self.inventory.pods();
        let counts = count(&pods);
        report.pods_pending = counts.pending;
        report.pods_running = counts.running;
        report.pods_suspended = counts.suspended;
        report.pods_other = counts.other;
        report.inventory_hash = inventory_hash(&pods);

        self.refresh().await;
        if let Some((_, installed, diskd)) = &self.slow {
            report.object_store = diskd.object_store;
            report.object_store_id = diskd.object_store_id.clone();
            if let Some(versions) = &mut report.versions {
                versions.firecracker = installed.firecracker.clone();
                versions.guest = installed.guest.clone();
                versions.diskd = diskd.version.clone();
            }
        }
        report
    }

    /// Asks for the installed versions and diskd again once a minute.
    async fn refresh(&mut self) {
        if let Some((at, _, _)) = &self.slow
            && at.elapsed() < SLOW
        {
            return;
        }
        let installed = self.inventory.installed();
        // A diskd that does not answer keeps what it said before.
        let diskd = match timeout(DISKD_TIMEOUT, self.inventory.diskd()).await {
            Ok(Some(diskd)) => diskd,
            _ => {
                warn!("Fleet: diskd does not answer; its object store is reported as before");
                self.slow
                    .take()
                    .map(|(_, _, diskd)| diskd)
                    .unwrap_or_default()
            }
        };
        self.slow = Some((Instant::now(), installed, diskd));
    }
}

/// The model of the first CPU and the number of CPUs, from /proc/cpuinfo.
fn cpuinfo(text: &str) -> (String, u32) {
    let mut model = None;
    let mut cores = 0;
    for line in text.lines() {
        let Some((key, value)) = line.split_once(':') else {
            continue;
        };
        match key.trim() {
            "processor" => cores += 1,
            "model name" if model.is_none() => model = Some(value.trim().to_string()),
            _ => {}
        }
    }
    (model.unwrap_or_default(), cores)
}

/// The load average over one minute, from /proc/loadavg.
fn loadavg(text: &str) -> f64 {
    text.split_whitespace()
        .next()
        .and_then(|load| load.parse().ok())
        .unwrap_or_default()
}

/// MemTotal and MemAvailable in bytes, from /proc/meminfo.
fn meminfo(text: &str) -> (u64, u64) {
    let field = |name: &str| {
        text.lines()
            .find_map(|line| line.strip_prefix(name)?.strip_prefix(':'))
            .and_then(|value| value.trim().strip_suffix("kB"))
            .and_then(|kib| kib.trim().parse::<u64>().ok())
            .map(|kib| kib * 1024)
            .unwrap_or_default()
    };
    (field("MemTotal"), field("MemAvailable"))
}

/// The size of the file system of the pods' disks, and what is free of it.
fn disk() -> (u64, u64) {
    for dir in DISK_DIRS {
        if let Ok(stat) = nix::sys::statvfs::statvfs(dir) {
            let fragment = stat.fragment_size();
            return (stat.blocks() * fragment, stat.blocks_available() * fragment);
        }
    }
    (0, 0)
}

#[derive(Debug, Default, PartialEq, Eq)]
struct Counts {
    pending: u32,
    running: u32,
    suspended: u32,
    other: u32,
}

fn count(pods: &[PodInfo]) -> Counts {
    let mut counts = Counts::default();
    for pod in pods {
        match pod.state() {
            PodState::Pending => counts.pending += 1,
            PodState::Running => counts.running += 1,
            PodState::Suspended => counts.suspended += 1,
            _ => counts.other += 1,
        }
    }
    counts
}

/// The hash of the node's steady pods: the SHA-256 of a line
/// "<pod id> <state>\n" per running or suspended pod, sorted by ID, which
/// the control plane computes from its registry too.
pub fn inventory_hash(pods: &[PodInfo]) -> String {
    let mut lines: Vec<(String, &str)> = pods
        .iter()
        .filter(|pod| matches!(pod.state(), PodState::Running | PodState::Suspended))
        .map(|pod| {
            let id = uuid::Uuid::parse_str(&pod.pod_id)
                .map(|id| id.hyphenated().to_string())
                .unwrap_or_else(|_| pod.pod_id.to_lowercase());
            (id, pod.state().as_str_name())
        })
        .collect();
    lines.sort();
    let text: String = lines
        .iter()
        .map(|(id, state)| format!("{id} {state}\n"))
        .collect();
    sha256_hex(text.as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pod(id: &str, state: PodState) -> PodInfo {
        PodInfo {
            pod_id: id.to_string(),
            state: state as i32,
            ..Default::default()
        }
    }

    #[test]
    fn the_inventory_hash_covers_the_steady_pods() {
        let a = "3f2c0b8e-1d2a-4c5b-9e7f-0a1b2c3d4e5f";
        let b = "0a6e5b44-7c21-4d0e-8f3a-9b8c7d6e5f40";
        let pods = [
            pod(a, PodState::Running),
            // Upper case is the same ID.
            pod(&b.to_uppercase(), PodState::Suspended),
            pod("11111111-2222-4333-8444-555555555555", PodState::Pending),
            pod("66666666-7777-4888-9999-aaaaaaaaaaaa", PodState::Hibernated),
            pod("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", PodState::Stopped),
        ];
        let text = format!("{b} POD_STATE_SUSPENDED\n{a} POD_STATE_RUNNING\n");
        assert_eq!(inventory_hash(&pods), sha256_hex(text.as_bytes()));
        // The order of the pods does not matter.
        let mut reversed = pods.clone();
        reversed.reverse();
        assert_eq!(inventory_hash(&reversed), inventory_hash(&pods));
        // No steady pods: the hash of nothing.
        assert_eq!(
            inventory_hash(&pods[2..]),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert_eq!(
            count(&pods),
            Counts {
                pending: 1,
                running: 1,
                suspended: 1,
                other: 2
            }
        );
    }

    #[test]
    fn proc_is_parsed() {
        let cpuinfo_text = "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz\nflags\t\t: fpu vme\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz\n";
        assert_eq!(
            cpuinfo(cpuinfo_text),
            ("Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz".to_string(), 2)
        );
        // aarch64 names no model.
        assert_eq!(
            cpuinfo("processor\t: 0\nBogoMIPS\t: 50.00\nCPU implementer\t: 0x41\n"),
            (String::new(), 1)
        );
        assert_eq!(loadavg("0.52 0.58 0.59 1/1234 56789\n"), 0.52);
        assert_eq!(loadavg(""), 0.0);
        let meminfo_text = "MemTotal:       16303428 kB\nMemFree:         1234567 kB\nMemAvailable:   12000000 kB\nBuffers:          100 kB\n";
        assert_eq!(meminfo(meminfo_text), (16303428 * 1024, 12000000 * 1024));
        assert_eq!(meminfo("MemTotal: x kB\n"), (0, 0));
    }
}
