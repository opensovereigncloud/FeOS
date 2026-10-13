// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use nix::sys::reboot::RebootMode;
use std::convert::Infallible;

/// Writes everything back to the disks and reboots. reboot(2) itself does
/// not sync.
pub fn reboot() -> Result<Infallible, nix::errno::Errno> {
    nix::unistd::sync();
    nix::sys::reboot::reboot(RebootMode::RB_AUTOBOOT)
}

/// Writes everything back to the disks and powers off.
pub fn shutdown() -> Result<Infallible, nix::errno::Errno> {
    nix::unistd::sync();
    nix::sys::reboot::reboot(RebootMode::RB_POWER_OFF)
}
