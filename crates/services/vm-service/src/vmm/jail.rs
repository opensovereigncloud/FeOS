// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Firecracker runs in the jailer. Each VM's directory is a chroot, in which
//! Firecracker runs as a user of the VM's own, in a mount namespace of its
//! own and under its seccomp filters. The jailer creates the device nodes
//! Firecracker needs; everything else it opens by path has to be in the
//! chroot:
//!
//! - the kernel, the initramfs and the root disk of the VM's image, and its
//!   data disks: a block device gets a device node of its own in the
//!   chroot, a file is hard linked or, from another filesystem, bind
//!   mounted; the VM's user owns the disks
//! - its configuration, its log, its sockets and its snapshot
//!
//! The VM's user also owns its TAP devices, which the VM service creates if
//! they do not exist. The jailer puts Firecracker into a cgroup of the VM's
//! own, `vms/<id>`. Outside the chroot, `jail.json` records the user, the
//! VM's share of the CPU, where the files come from and which TAP devices
//! the VM service created, so that a start can link them again and a
//! deletion can undo it all.

use crate::{jail_dir, vm_dir, VM_DIR};
use log::warn;
use nix::mount::{mount, umount2, MntFlags, MsFlags};
use nix::sys::stat::{mknod, Mode, SFlag};
use nix::unistd::{chown, Gid, Uid};
use serde_json::{json, Value};
use std::fs::{self, File};
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};

/// The first VM's user and group ID.
const UID_BASE: u32 = 1_000_000;
const MANIFEST: &str = "jail.json";
const CGROUP_ROOT: &str = "/sys/fs/cgroup";
/// The cgroup below the root that holds a cgroup per VM.
pub const PARENT_CGROUP: &str = "vms";

/// A file Firecracker opens: where it comes from, and its name in the
/// chroot.
#[derive(Debug, Clone, PartialEq)]
pub struct Link {
    pub source: PathBuf,
    pub name: String,
    /// A disk, which the VM's user owns.
    pub disk: bool,
}

impl Link {
    /// The path Firecracker opens.
    pub fn jailed(&self) -> String {
        format!("/{}", self.name)
    }
}

/// The share of the CPU that a VM with `vcpus` gets, its `cpu.weight`:
/// VMs share the CPU in proportion to their vCPUs, as their vCPU threads
/// would without a cgroup each.
pub fn cpu_weight(vcpus: u32) -> u64 {
    // cgroup v2 takes weights from 1 to 10000.
    (100 * u64::from(vcpus)).clamp(1, 10_000)
}

/// The jailer's `--cgroup` values for a VM with `cpu_weight`, none for a
/// VM from before cgroups. `memory.max=max` turns the memory controller on
/// for the VM's cgroup, which then accounts its memory, without a cap:
/// where /var/lib/feos is a tmpfs, what the VM writes to its disks and its
/// snapshot in there is charged to it, and a cap would kill the VM when its
/// guest fills its disk.
pub fn cgroup_values(cpu_weight: u64) -> Vec<String> {
    if cpu_weight == 0 {
        return Vec::new();
    }
    vec![
        format!("cpu.weight={cpu_weight}"),
        "memory.max=max".to_string(),
    ]
}

/// The VM's cgroup.
pub fn cgroup(vm_id: &str) -> PathBuf {
    Path::new(CGROUP_ROOT).join(PARENT_CGROUP).join(vm_id)
}

/// What the VM service keeps about a jail.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Manifest {
    pub uid: u32,
    /// The VM's `cpu.weight`; 0 for a VM from before cgroups.
    pub cpu_weight: u64,
    pub links: Vec<Link>,
    pub taps: Vec<String>,
    /// The TAP devices the VM service created, and deletes with the VM.
    pub created_taps: Vec<String>,
}

impl Manifest {
    pub fn save(&self, vm_id: &str) -> io::Result<()> {
        self.write(&jail_dir(vm_id).join(MANIFEST))
    }

    fn write(&self, path: &Path) -> io::Result<()> {
        let links: Vec<Value> = self
            .links
            .iter()
            .map(|link| json!({"source": link.source, "name": link.name, "disk": link.disk}))
            .collect();
        let manifest = json!({
            "uid": self.uid,
            "cpu_weight": self.cpu_weight,
            "links": links,
            "taps": self.taps,
            "created_taps": self.created_taps,
        });
        fs::write(path, manifest.to_string())
    }

    pub fn load(vm_id: &str) -> io::Result<Self> {
        Self::read(&jail_dir(vm_id).join(MANIFEST))
    }

    fn read(path: &Path) -> io::Result<Self> {
        let manifest: Value = serde_json::from_slice(&fs::read(path)?).map_err(io::Error::other)?;
        let strings = |value: &Value| -> Vec<String> {
            value
                .as_array()
                .into_iter()
                .flatten()
                .filter_map(|s| s.as_str().map(str::to_string))
                .collect()
        };
        let links = manifest["links"]
            .as_array()
            .into_iter()
            .flatten()
            .map(|link| Link {
                source: PathBuf::from(link["source"].as_str().unwrap_or_default()),
                name: link["name"].as_str().unwrap_or_default().to_string(),
                disk: link["disk"].as_bool().unwrap_or(false),
            })
            .collect();
        Ok(Self {
            uid: manifest["uid"]
                .as_u64()
                .and_then(|uid| u32::try_from(uid).ok())
                .ok_or_else(|| io::Error::other("jail.json has no uid"))?,
            cpu_weight: manifest["cpu_weight"].as_u64().unwrap_or(0),
            links,
            taps: strings(&manifest["taps"]),
            created_taps: strings(&manifest["created_taps"]),
        })
    }
}

/// The lowest user ID no other jail has. The caller serializes allocations.
pub fn allocate_uid() -> io::Result<u32> {
    let jails = Path::new(VM_DIR).join(crate::FIRECRACKER_BIN);
    let mut taken = Vec::new();
    match fs::read_dir(&jails) {
        Ok(entries) => {
            for entry in entries.flatten() {
                if let Ok(manifest) = Manifest::read(&entry.path().join(MANIFEST)) {
                    taken.push(manifest.uid);
                }
            }
        }
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }
    (UID_BASE..u32::MAX)
        .find(|uid| !taken.contains(uid))
        .ok_or_else(|| io::Error::other("no user ID left"))
}

/// Gives the VM's user a file it creates in, such as the snapshot directory.
pub fn give(vm_id: &str, path: &Path) -> io::Result<()> {
    let uid = Manifest::load(vm_id)?.uid;
    Ok(chown(
        path,
        Some(Uid::from_raw(uid)),
        Some(Gid::from_raw(uid)),
    )?)
}

/// Readies the jail for a start of the VM: links its files in, gives the
/// VM's user its log and its TAP devices, and removes what the jailer
/// creates and refuses to find.
pub fn prepare(vm_id: &str) -> io::Result<()> {
    let mut manifest = Manifest::load(vm_id)?;
    let (uid, gid) = (Uid::from_raw(manifest.uid), Gid::from_raw(manifest.uid));
    let root = vm_dir(vm_id);
    for link in &manifest.links {
        place(&root, link, manifest.uid)?;
    }

    let log = root.join("vmm.log");
    File::options().create(true).append(true).open(&log)?;
    chown(&log, Some(uid), Some(gid))?;

    let mut created = false;
    for name in &manifest.taps {
        if tap::ensure(name, manifest.uid)? && !manifest.created_taps.contains(name) {
            manifest.created_taps.push(name.clone());
            created = true;
        }
    }
    if created {
        manifest.save(vm_id)?;
    }

    remove(&root.join("dev"))?;
    remove(&root.join(format!("{}.pid", crate::FIRECRACKER_BIN)))?;
    Ok(())
}

/// Undoes what `prepare` and the jailer did outside the jail, for a VM
/// that is gone.
pub fn release(vm_id: &str) {
    match fs::remove_dir(cgroup(vm_id)) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => {
            warn!("Jail ({vm_id}): Failed to remove its cgroup: {e}")
        }
        _ => {}
    }
    let Ok(manifest) = Manifest::load(vm_id) else {
        return;
    };
    let root = vm_dir(vm_id);
    for link in &manifest.links {
        if let Err(e) = unlink(&root.join(&link.name)) {
            warn!("Jail ({vm_id}): Failed to remove {}: {e}", link.name);
        }
    }
    for name in &manifest.created_taps {
        if let Err(e) = tap::delete(name) {
            warn!("Jail ({vm_id}): Failed to delete TAP device {name}: {e}");
        }
    }
}

/// Puts `link` into the chroot `root`.
fn place(root: &Path, link: &Link, uid: u32) -> io::Result<()> {
    let target = root.join(&link.name);
    unlink(&target)?;
    let (uid, gid) = (Uid::from_raw(uid), Gid::from_raw(uid));
    let source = fs::metadata(&link.source)
        .map_err(|e| io::Error::new(e.kind(), format!("{}: {e}", link.source.display())))?;
    if source.file_type().is_block_device() {
        // A node of the VM's own leaves the host's alone.
        mknod(
            &target,
            SFlag::S_IFBLK,
            Mode::S_IRUSR | Mode::S_IWUSR,
            source.rdev(),
        )?;
        chown(&target, Some(uid), Some(gid))?;
        return Ok(());
    }
    match fs::hard_link(&link.source, &target) {
        Ok(()) => {}
        Err(e) if e.raw_os_error() == Some(libc::EXDEV) => {
            File::create(&target)?;
            mount(
                Some(&link.source),
                &target,
                None::<&str>,
                MsFlags::MS_BIND,
                None::<&str>,
            )?;
        }
        Err(e) => return Err(e),
    }
    if link.disk {
        chown(&link.source, Some(uid), Some(gid))?;
    }
    Ok(())
}

/// Removes a file from the chroot, unmounting it first if it is bind
/// mounted from another filesystem.
fn unlink(target: &Path) -> io::Result<()> {
    let meta = match fs::symlink_metadata(target) {
        Ok(meta) => meta,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    let parent = target.parent().unwrap_or(Path::new("/"));
    if meta.dev() != fs::metadata(parent)?.dev() {
        match umount2(target, MntFlags::MNT_DETACH) {
            Ok(()) | Err(nix::Error::EINVAL) => {}
            Err(e) => return Err(e.into()),
        }
    }
    fs::remove_file(target)
}

fn remove(path: &Path) -> io::Result<()> {
    let removed = if path.is_dir() {
        fs::remove_dir_all(path)
    } else {
        fs::remove_file(path)
    };
    match removed {
        Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
        _ => Ok(()),
    }
}

/// TAP devices, through the TUN driver's ioctls.
pub mod tap {
    use nix::fcntl::{open, OFlag};
    use nix::sys::stat::Mode;
    use std::io;
    use std::os::fd::{AsRawFd, OwnedFd};
    use std::path::Path;

    const TUN_TYPE: u8 = b'T';
    nix::ioctl_write_ptr_bad!(
        tunsetiff,
        nix::request_code_write!(TUN_TYPE, 202, size_of::<libc::c_int>()),
        libc::ifreq
    );
    nix::ioctl_write_int_bad!(
        tunsetpersist,
        nix::request_code_write!(TUN_TYPE, 203, size_of::<libc::c_int>())
    );
    nix::ioctl_write_int_bad!(
        tunsetowner,
        nix::request_code_write!(TUN_TYPE, 204, size_of::<libc::c_int>())
    );
    nix::ioctl_write_int_bad!(
        tunsetgroup,
        nix::request_code_write!(TUN_TYPE, 206, size_of::<libc::c_int>())
    );

    /// Opens the TAP device `name`, creating it if it does not exist.
    fn attach(name: &str) -> io::Result<OwnedFd> {
        if name.is_empty() || name.len() >= libc::IFNAMSIZ {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("bad TAP device name {name:?}"),
            ));
        }
        let fd = open(
            "/dev/net/tun",
            OFlag::O_RDWR | OFlag::O_CLOEXEC,
            Mode::empty(),
        )?;
        // SAFETY: ifreq is plain data; all zeroes is a valid value.
        let mut ifr: libc::ifreq = unsafe { std::mem::zeroed() };
        for (dst, src) in ifr.ifr_name.iter_mut().zip(name.bytes()) {
            *dst = src as libc::c_char;
        }
        ifr.ifr_ifru.ifru_flags = (libc::IFF_TAP | libc::IFF_NO_PI) as libc::c_short;
        // SAFETY: the fd is the TUN device and ifr outlives the call.
        unsafe { tunsetiff(fd.as_raw_fd(), &ifr) }?;
        Ok(fd)
    }

    /// Makes the TAP device `name` persistent and `uid`'s, so that a
    /// Firecracker running as `uid` can attach to it. Returns whether it
    /// was created.
    pub fn ensure(name: &str, uid: u32) -> io::Result<bool> {
        let existed = Path::new("/sys/class/net").join(name).exists();
        let fd = attach(name)?;
        let raw = fd.as_raw_fd();
        // SAFETY: the fd is attached to the TAP device.
        unsafe {
            tunsetowner(raw, uid as libc::c_int)?;
            tunsetgroup(raw, uid as libc::c_int)?;
            tunsetpersist(raw, 1)?;
        }
        Ok(!existed)
    }

    /// Deletes the persistent TAP device `name`.
    pub fn delete(name: &str) -> io::Result<()> {
        if !Path::new("/sys/class/net").join(name).exists() {
            return Ok(());
        }
        let fd = attach(name)?;
        // SAFETY: the fd is attached to the TAP device; it goes when the fd
        // closes.
        unsafe { tunsetpersist(fd.as_raw_fd(), 0) }?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_manifest_survives_a_round_trip() {
        let dir = tempfile::tempdir().unwrap();
        let manifest = Manifest {
            uid: UID_BASE + 3,
            cpu_weight: cpu_weight(2),
            links: vec![
                Link {
                    source: PathBuf::from("/var/lib/feos/images/x/vmlinuz"),
                    name: "vmlinuz".to_string(),
                    disk: false,
                },
                Link {
                    source: PathBuf::from("/data.img"),
                    name: "disk0.img".to_string(),
                    disk: true,
                },
            ],
            taps: vec!["tap0".to_string()],
            created_taps: vec![],
        };
        let path = dir.path().join(MANIFEST);
        manifest.write(&path).unwrap();
        assert_eq!(Manifest::read(&path).unwrap(), manifest);
        assert_eq!(manifest.links[1].jailed(), "/disk0.img");
    }

    #[test]
    fn vms_share_the_cpu_by_their_vcpus() {
        assert_eq!(
            cgroup_values(cpu_weight(2)),
            vec!["cpu.weight=200".to_string(), "memory.max=max".to_string()]
        );
        assert_eq!(cpu_weight(0), 1);
        assert_eq!(cpu_weight(1000), 10_000);
        // A manifest from before cgroups puts the VM in none.
        assert!(cgroup_values(0).is_empty());
    }

    #[test]
    fn a_file_is_linked_into_the_chroot_and_removed_again() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("root");
        fs::create_dir(&root).unwrap();
        let source = dir.path().join("vmlinuz");
        fs::write(&source, b"kernel").unwrap();
        let link = Link {
            source: source.clone(),
            name: "vmlinuz".to_string(),
            disk: false,
        };
        let uid = nix::unistd::getuid().as_raw();
        place(&root, &link, uid).unwrap();
        // Again, as every start does.
        place(&root, &link, uid).unwrap();
        assert_eq!(fs::read(root.join("vmlinuz")).unwrap(), b"kernel");
        unlink(&root.join("vmlinuz")).unwrap();
        assert!(!root.join("vmlinuz").exists());
        assert!(source.exists());
    }
}
