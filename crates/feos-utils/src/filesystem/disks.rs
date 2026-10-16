// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A microVM's virtio disks, in the order of its drives (ADR 0016).
//!
//! Firecracker attaches a pod's drives in the order of its configuration,
//! the pod's disk and then its volumes, and gives each the next MMIO
//! window; the guest's kernel finds them through ACPI or its command line.
//! Firecracker answers no drive's ID: the guest orders its virtio disks by
//! the address of their window, and where that is not to be had, by the
//! index the kernel gave their virtio devices, which follows the order in
//! which it found them.

use std::path::Path;

/// A virtio disk of the microVM.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Disk {
    /// The kernel's name of it, e.g. `vdb`.
    pub name: String,
    pub size_bytes: u64,
    /// The start of its MMIO window, if the guest found it.
    pub window: Option<u64>,
}

impl Disk {
    pub fn path(&self) -> String {
        format!("/dev/{}", self.name)
    }
}

/// The virtio disks there are, in the order of the microVM's drives.
pub fn virtio_disks() -> Vec<Disk> {
    let Ok(entries) = std::fs::read_dir("/sys/block") else {
        return Vec::new();
    };
    let iomem = std::fs::read_to_string("/proc/iomem").unwrap_or_default();
    let mut disks = Vec::new();
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if !name.starts_with("vd") {
            continue;
        }
        let block = entry.path();
        let size_bytes = std::fs::read_to_string(block.join("size"))
            .ok()
            .and_then(|sectors| sectors.trim().parse::<u64>().ok())
            .map_or(0, |sectors| sectors * 512);
        // .../<transport>/virtio<N>, the transport being the platform
        // device of the MMIO window, e.g. LNRO0005:01 or virtio-mmio.1.
        let device = std::fs::canonicalize(block.join("device")).ok();
        let index = device
            .as_deref()
            .and_then(Path::file_name)
            .and_then(|virtio| virtio.to_str()?.strip_prefix("virtio")?.parse().ok());
        let window = device
            .as_deref()
            .and_then(Path::parent)
            .and_then(Path::file_name)
            .and_then(|transport| window_start(&iomem, &transport.to_string_lossy()));
        disks.push((
            Order {
                window,
                index,
                name: name.clone(),
            },
            Disk {
                name,
                size_bytes,
                window,
            },
        ));
    }
    in_drive_order(disks)
}

/// What a disk's place among the drives is told by.
#[derive(Debug, Clone)]
struct Order {
    /// The start of its MMIO window.
    window: Option<u64>,
    /// The index of its virtio device.
    index: Option<u64>,
    name: String,
}

/// Orders the disks by the address of their MMIO windows if every one has
/// one, else by their virtio devices' indexes if every one has one, else
/// by their names as the kernel hands them out: vda, ..., vdz, vdaa.
fn in_drive_order(mut disks: Vec<(Order, Disk)>) -> Vec<Disk> {
    if disks.iter().all(|(order, _)| order.window.is_some()) {
        disks.sort_by_key(|(order, _)| order.window);
    } else if disks.iter().all(|(order, _)| order.index.is_some()) {
        disks.sort_by_key(|(order, _)| order.index);
    } else {
        disks.sort_by(|(a, _), (b, _)| (a.name.len(), &a.name).cmp(&(b.name.len(), &b.name)));
    }
    disks.into_iter().map(|(_, disk)| disk).collect()
}

/// The start of the window that /proc/iomem names `device`'s.
fn window_start(iomem: &str, device: &str) -> Option<u64> {
    iomem.lines().find_map(|line| {
        let (range, name) = line.trim().split_once(" : ")?;
        if name.trim() != device {
            return None;
        }
        let (start, _) = range.split_once('-')?;
        u64::from_str_radix(start, 16).ok()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn disk(name: &str, window: Option<u64>, index: Option<u64>) -> (Order, Disk) {
        (
            Order {
                window,
                index,
                name: name.to_string(),
            },
            Disk {
                name: name.to_string(),
                size_bytes: 0,
                window,
            },
        )
    }

    fn names(disks: Vec<Disk>) -> Vec<String> {
        disks.into_iter().map(|disk| disk.name).collect()
    }

    #[test]
    fn windows_name_their_devices() {
        let iomem = "\
00000000-00000fff : Reserved
c0001000-c0001fff : LNRO0005:00
  c0001000-c0001fff : LNRO0005:00
c0002000-c0002fff : LNRO0005:01
c000a000-c000afff : virtio-mmio.9
";
        assert_eq!(window_start(iomem, "LNRO0005:00"), Some(0xc000_1000));
        assert_eq!(window_start(iomem, "LNRO0005:01"), Some(0xc000_2000));
        assert_eq!(window_start(iomem, "virtio-mmio.9"), Some(0xc000_a000));
        assert_eq!(window_start(iomem, "LNRO0005:02"), None);
    }

    #[test]
    fn disks_are_in_the_order_of_their_windows() {
        // The kernel named them in another order than Firecracker
        // attached them.
        let disks = vec![
            disk("vda", Some(0xc000_3000), Some(3)),
            disk("vdb", Some(0xc000_2000), Some(1)),
            disk("vdc", Some(0xc000_4000), Some(2)),
        ];
        assert_eq!(names(in_drive_order(disks)), ["vdb", "vda", "vdc"]);
    }

    #[test]
    fn without_windows_the_virtio_index_orders() {
        let disks = vec![
            disk("vda", None, Some(3)),
            disk("vdb", Some(0xc000_2000), Some(1)),
            disk("vdc", None, Some(2)),
        ];
        assert_eq!(names(in_drive_order(disks)), ["vdb", "vdc", "vda"]);
    }

    #[test]
    fn without_either_the_names_order() {
        let disks = vec![
            disk("vdaa", None, None),
            disk("vdb", None, Some(1)),
            disk("vdz", None, None),
            disk("vda", None, None),
        ];
        assert_eq!(names(in_drive_order(disks)), ["vda", "vdb", "vdz", "vdaa"]);
    }
}
