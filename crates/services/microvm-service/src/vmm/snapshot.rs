// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The snapshot of a suspended VM, in `snapshot/` in the VM's directory:
//!
//! - `vmstate`: the state of the VM's devices and vCPUs
//! - `memory`: the guest's memory, with holes where it was all zeroes
//! - `host.json`: what the snapshot needs of the host that restores it
//!
//! A snapshot written elsewhere (`Remote`) has no memory file: a
//! userfaultfd handler serves the memory on `memory.sock` in the VM's
//! directory while the guest runs.
//!
//! A snapshot is restored once. Once the restored VM ran, its disks have
//! moved on, and the snapshot would roll the guest back against them.

use super::{installed, jail};
use crate::vm_dir;
use nix::fcntl::{fallocate, FallocateFlags};
use nix::unistd::{lseek, Whence};
use serde_json::{json, Value};
use std::fs::File;
use std::io;
use std::os::unix::fs::{FileExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::time::Duration;

const PAGE: usize = 4096;
const ZERO_PAGE: [u8; PAGE] = [0; PAGE];
/// How much of the memory file is read at a time.
const CHUNK: usize = 1 << 20;

/// Where Firecracker, in its jail, sees the snapshot.
pub const JAILED_VMSTATE: &str = "/snapshot/vmstate";
pub const JAILED_MEMORY: &str = "/snapshot/memory";
/// The userfaultfd handler's socket of a remote snapshot, in the VM's
/// directory.
pub const MEMORY_SOCKET: &str = "memory.sock";

/// A snapshot written elsewhere: its vmstate, a file on the node, and what
/// it was written with. Its memory comes from a userfaultfd handler.
#[derive(Debug, Clone)]
pub struct Remote {
    pub vmstate: PathBuf,
    pub host: Host,
}

pub fn dir(vm_id: &str) -> PathBuf {
    vm_dir(vm_id).join("snapshot")
}

pub fn vmstate(vm_id: &str) -> PathBuf {
    dir(vm_id).join("vmstate")
}

pub fn memory(vm_id: &str) -> PathBuf {
    dir(vm_id).join("memory")
}

fn host_file(vm_id: &str) -> PathBuf {
    dir(vm_id).join("host.json")
}

pub fn exists(vm_id: &str) -> bool {
    vmstate(vm_id).is_file() && memory(vm_id).is_file()
}

pub fn remove(vm_id: &str) -> io::Result<()> {
    match std::fs::remove_dir_all(dir(vm_id)) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
        _ => Ok(()),
    }
}

/// Takes a remote snapshot into the VM's directory: its vmstate, moved
/// there, and what it was written with, for the restore to check. Both
/// are the VM's user's.
pub fn receive(vm_id: &str, remote: &Remote) -> io::Result<()> {
    remove(vm_id)?;
    std::fs::create_dir_all(dir(vm_id))?;
    let target = vmstate(vm_id);
    match std::fs::rename(&remote.vmstate, &target) {
        Ok(()) => {}
        // From another filesystem.
        Err(e) if e.raw_os_error() == Some(libc::EXDEV) => {
            std::fs::copy(&remote.vmstate, &target)?;
            std::fs::remove_file(&remote.vmstate)?;
        }
        Err(e) => return Err(e),
    }
    remote.host.save(vm_id)?;
    for path in [dir(vm_id), target] {
        jail::give(vm_id, &path)?;
    }
    Ok(())
}

/// What a snapshot needs of the host that restores it: the Firecracker that
/// wrote it, the CPU model whose features the guest saw, and the kernel
/// whose KVM saved the vCPUs.
#[derive(Debug, Clone, PartialEq)]
pub struct Host {
    pub firecracker_version: String,
    pub cpu_model: String,
    pub kernel_release: String,
}

impl Host {
    /// This host, running `firecracker_version`.
    pub fn this(firecracker_version: &str) -> Self {
        let cpuinfo = std::fs::read_to_string("/proc/cpuinfo").unwrap_or_default();
        Self {
            firecracker_version: firecracker_version.to_string(),
            cpu_model: cpu_model(&cpuinfo),
            kernel_release: nix::sys::utsname::uname()
                .map(|uts| uts.release().to_string_lossy().into_owned())
                .unwrap_or_default(),
        }
    }

    pub fn save(&self, vm_id: &str) -> io::Result<()> {
        let host = json!({
            "firecracker_version": self.firecracker_version,
            "cpu_model": self.cpu_model,
            "kernel_release": self.kernel_release,
        });
        std::fs::write(host_file(vm_id), host.to_string())
    }

    pub fn load(vm_id: &str) -> io::Result<Self> {
        let host: Value =
            serde_json::from_slice(&std::fs::read(host_file(vm_id))?).map_err(io::Error::other)?;
        let field = |name: &str| host[name].as_str().unwrap_or_default().to_string();
        Ok(Self {
            firecracker_version: field("firecracker_version"),
            cpu_model: field("cpu_model"),
            kernel_release: field("kernel_release"),
        })
    }

    /// Why a snapshot written on `self` cannot be restored on this host:
    /// it needs the Firecracker that wrote it installed, and the same CPU
    /// model and kernel. What a snapshot does not record counts as
    /// different.
    pub fn unrestorable_here(&self) -> Option<String> {
        if installed::firecracker(&self.firecracker_version).is_none() {
            return Some(format!(
                "the snapshot is from Firecracker {:?}, which is not installed",
                self.firecracker_version
            ));
        }
        self.mismatch(&Host::this(&self.firecracker_version))
    }

    /// Why a snapshot written on `self` cannot be restored on `other`.
    pub fn mismatch(&self, other: &Self) -> Option<String> {
        let differs = |what: &str, ours: &str, theirs: &str| {
            (ours != theirs).then(|| format!("{what} {ours:?}, this host has {theirs:?}"))
        };
        differs(
            "the snapshot is from Firecracker",
            &self.firecracker_version,
            &other.firecracker_version,
        )
        .or_else(|| {
            differs(
                "the snapshot is from CPU",
                &self.cpu_model,
                &other.cpu_model,
            )
        })
        .or_else(|| {
            differs(
                "the snapshot is from kernel",
                &self.kernel_release,
                &other.kernel_release,
            )
        })
    }
}

fn cpu_model(cpuinfo: &str) -> String {
    cpuinfo
        .lines()
        .find_map(|line| {
            let (key, value) = line.split_once(':')?;
            (key.trim() == "model name").then(|| value.trim().to_string())
        })
        .unwrap_or_default()
}

/// The bytes a file takes on disk.
pub fn disk_usage(path: &Path) -> io::Result<u64> {
    Ok(std::fs::metadata(path)?.blocks() * 512)
}

/// Punches holes where the file has pages of zeroes, which Firecracker
/// writes for memory the guest freed. A hole reads back as zeroes, so the
/// restored guest sees the same memory. Returns the bytes punched.
pub fn punch_zero_pages(path: &Path) -> io::Result<u64> {
    let file = File::options().read(true).write(true).open(path)?;
    let fd = &file;
    let len = file.metadata()?.len() as i64;
    let mut buf = vec![0u8; CHUNK];
    let mut punched = 0u64;
    let mut offset = 0i64;
    // Only the data extents can hold pages of zeroes.
    while offset < len {
        let data = match lseek(fd, offset, Whence::SeekData) {
            Ok(data) => data,
            Err(nix::Error::ENXIO) => break,
            Err(e) => return Err(e.into()),
        };
        let hole = lseek(fd, data, Whence::SeekHole)?;
        let mut position = data;
        while position < hole {
            let n = CHUNK.min((hole - position) as usize);
            file.read_exact_at(&mut buf[..n], position as u64)?;
            // Runs of zero pages, punched at once.
            let mut run: Option<i64> = None;
            for (index, page) in buf[..n].chunks(PAGE).enumerate() {
                let page_offset = position + (index * PAGE) as i64;
                // A hole smaller than a page frees nothing.
                match (page == ZERO_PAGE, run) {
                    (true, None) => run = Some(page_offset),
                    (false, Some(start)) => {
                        punched += punch(&file, start, page_offset)?;
                        run = None;
                    }
                    _ => {}
                }
            }
            let end = position + n as i64;
            if let Some(start) = run {
                punched += punch(&file, start, end)?;
            }
            position = end;
        }
        offset = hole;
    }
    Ok(punched)
}

fn punch(file: &File, start: i64, end: i64) -> io::Result<u64> {
    let flags = FallocateFlags::FALLOC_FL_PUNCH_HOLE | FallocateFlags::FALLOC_FL_KEEP_SIZE;
    fallocate(file, flags, start, end - start)?;
    Ok((end - start) as u64)
}

/// The resident memory of a process, in KiB.
fn resident_kib(pid: i32) -> Option<u64> {
    let status = std::fs::read_to_string(format!("/proc/{pid}/status")).ok()?;
    status
        .lines()
        .find_map(|line| line.strip_prefix("VmRSS:"))?
        .trim()
        .trim_end_matches("kB")
        .trim()
        .parse()
        .ok()
}

/// Waits until free page reporting has handed the guest's free memory to
/// the VMM, which drops it: until the VMM's resident memory shrinks by less
/// than 2 % three times in a row, at most 3 s.
pub async fn settle_free_pages(pid: i32) {
    const INTERVAL: Duration = Duration::from_millis(250);
    const LIMIT: Duration = Duration::from_secs(3);
    let deadline = tokio::time::Instant::now() + LIMIT;
    let Some(mut last) = resident_kib(pid) else {
        return;
    };
    let mut steady = 0;
    while steady < 3 && tokio::time::Instant::now() < deadline {
        tokio::time::sleep(INTERVAL).await;
        let Some(now) = resident_kib(pid) else {
            return;
        };
        steady = if now * 50 >= last * 49 { steady + 1 } else { 0 };
        last = now;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn zero_pages_become_holes() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("memory");
        // data, zero, zero, data, and a short page of zeroes.
        let mut content = Vec::new();
        content.extend_from_slice(&[1u8; PAGE]);
        content.extend_from_slice(&[0u8; 2 * PAGE]);
        content.extend_from_slice(&[2u8; PAGE]);
        content.extend_from_slice(&[0u8; 100]);
        std::fs::write(&path, &content).unwrap();

        let punched = punch_zero_pages(&path).unwrap();
        assert_eq!(punched, 2 * PAGE as u64);
        assert_eq!(std::fs::read(&path).unwrap(), content);
        // A second pass finds nothing left to punch.
        assert_eq!(punch_zero_pages(&path).unwrap(), 0);
    }

    #[test]
    fn a_file_of_holes_is_left_alone() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("memory");
        File::create(&path).unwrap().set_len(1 << 20).unwrap();
        assert_eq!(punch_zero_pages(&path).unwrap(), 0);
    }

    #[test]
    fn the_cpu_model_comes_from_cpuinfo() {
        let cpuinfo = "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD EPYC 7B13\n";
        assert_eq!(cpu_model(cpuinfo), "AMD EPYC 7B13");
    }

    #[test]
    fn another_host_is_told_apart() {
        let host = Host {
            firecracker_version: "1.17.0".to_string(),
            cpu_model: "A".to_string(),
            kernel_release: "6.12".to_string(),
        };
        assert_eq!(host.mismatch(&host.clone()), None);
        let other = Host {
            cpu_model: "B".to_string(),
            ..host.clone()
        };
        assert!(host.mismatch(&other).unwrap().contains("CPU"));
    }
}
