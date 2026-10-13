// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

mod data_volume;
mod fsmount;
mod mount;
mod r#move;

pub use data_volume::mount_data_volume;

/// Where FeOS keeps its state: images, containers, databases. A guest mounts
/// its data volume here; a host keeps it in RAM.
pub const DATA_ROOT: &str = "/var/lib/feos";
/// One directory per container: its bundle, overlay layers and logs.
pub const CONTAINERS_DIR: &str = "/var/lib/feos/containers";
pub use mount::mount_virtual_filesystems;
pub use r#move::{get_root_fstype, move_root};
