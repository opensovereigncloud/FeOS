// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The suspend's budget: how long a SuspendPod may take on this node, from
//! its call to its answer, for a VM of a size. A caller's deadline covers
//! it, so that feosd answers, the suspend's success or `UNAVAILABLE` "the
//! VM is busy", before its caller gives up: ferund's pause waits this
//! budget and 30 s more (docs/architecture/node.md, "The suspend's
//! budget"; control-plane.md, "Ops"). Both follow from one formula:
//!
//! - before the snapshot, 40 s: the guest empties its memory
//!   (`guest::PUSH_TIMEOUT`, 10 s), free page reporting settles (3 s),
//!   Firecracker's API reads and pauses the VM (`fc_api::REQUEST_TIMEOUT`
//!   each, 10 s), and 7 s for the snapshot's directory;
//! - the snapshot, `snapshot_timeout`: 120 s and 4 s per GiB;
//! - after it, 30 s and 1 s per GiB: Firecracker's version
//!   (`fc_api::REQUEST_TIMEOUT`), its end (`KILL_TIMEOUT`, 5 s), the VM's
//!   record (`RECORD_SETTLE`, 10 s), and 5 s for the snapshot's host
//!   record, the pod's record and its disk; per GiB, the zeroes punched out
//!   of the memory file, which is read once.
//!
//! In all, 190 s and 5 s per GiB, where GiB is the VM's memory rounded up.
//! 4 s per GiB is a write of 256 MiB/s, which a node's disk sustains while
//! it writes back other memory; a snapshot that is slower is late, fails
//! its suspend as busy at its deadline, and is left to Firecracker
//! (`firecracker`).

use std::time::Duration;

/// What a snapshot may take whatever the VM's size.
pub const SNAPSHOT_BASE: Duration = Duration::from_secs(120);
/// What it may take more per GiB of the VM's memory.
pub const SNAPSHOT_PER_GIB: Duration = Duration::from_secs(4);
/// How long free page reporting may take to hand the guest's free memory
/// back before a snapshot (`snapshot::settle_free_pages`).
pub const FREE_PAGES_SETTLE: Duration = Duration::from_secs(3);
/// How long the microVM service may take to record a change it made, after
/// the call that made it answered: the pods' service waits as long.
pub const RECORD_SETTLE: Duration = Duration::from_secs(10);
/// What the steps before the snapshot may take, and the steps after it
/// whatever the VM's size, and per GiB.
pub const BEFORE_SNAPSHOT: Duration = Duration::from_secs(40);
pub const AFTER_SNAPSHOT: Duration = Duration::from_secs(30);
pub const AFTER_PER_GIB: Duration = Duration::from_secs(1);
/// The memory of a VM whose configuration names none (Firecracker's).
const DEFAULT_MEMORY_MIB: u64 = 512;

/// The VM's memory in GiB, rounded up.
fn gib(memory_mib: u64) -> u32 {
    let memory_mib = if memory_mib == 0 {
        DEFAULT_MEMORY_MIB
    } else {
        memory_mib
    };
    u32::try_from(memory_mib.div_ceil(1024)).unwrap_or(u32::MAX)
}

/// How long a suspend waits for the snapshot of a VM of `memory_mib`.
pub fn snapshot_timeout(memory_mib: u64) -> Duration {
    SNAPSHOT_BASE.saturating_add(SNAPSHOT_PER_GIB.saturating_mul(gib(memory_mib)))
}

/// How long a SuspendPod of a VM of `memory_mib` may take, from its call to
/// its answer.
pub fn suspend_budget(memory_mib: u64) -> Duration {
    BEFORE_SNAPSHOT
        .saturating_add(snapshot_timeout(memory_mib))
        .saturating_add(AFTER_SNAPSHOT)
        .saturating_add(AFTER_PER_GIB.saturating_mul(gib(memory_mib)))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The budget is the one control-plane.md states and ferund's pause
    /// deadline is made of (go/internal/sandbox, feosdSuspendBudget, whose
    /// test has the same table): 190 s and 5 s per GiB, of which the
    /// snapshot 120 s and 4 s per GiB.
    #[test]
    fn the_budget_is_the_one_ferund_waits_for() {
        for (memory_mib, snapshot, budget) in [
            (0, 124, 195),
            (512, 124, 195),
            (1024, 124, 195),
            (1025, 128, 200),
            (4096, 136, 210),
            (16384, 184, 270),
        ] {
            assert_eq!(
                snapshot_timeout(memory_mib),
                Duration::from_secs(snapshot),
                "the snapshot of {memory_mib} MiB"
            );
            assert_eq!(
                suspend_budget(memory_mib),
                Duration::from_secs(budget),
                "the suspend of {memory_mib} MiB"
            );
        }
    }

    /// Every bounded step of a suspend fits in its part of the budget.
    #[test]
    fn the_budget_holds_the_steps_of_a_suspend() {
        let request = crate::vmm::fc_api::REQUEST_TIMEOUT;
        let before = crate::guest::PUSH_TIMEOUT + FREE_PAGES_SETTLE + request * 2;
        assert!(before < BEFORE_SNAPSHOT, "{before:?}");
        let after = request + crate::vmm::firecracker::KILL_TIMEOUT + RECORD_SETTLE;
        assert!(after < AFTER_SNAPSHOT, "{after:?}");
    }
}
