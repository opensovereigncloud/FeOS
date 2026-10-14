# ADR 0008: Suspend and restore

**Status:** Accepted · **Date:** 2026-10-13

## Context

ADR 0001 asks for cheap suspends, fast resumes, memory back on suspend and
resumes on other nodes: the sandboxes are idle most of the time, and a
service of CaaS that nobody calls should scale to zero. Firecracker
writes snapshots of a VM's state and memory and restores them, but only
on the Firecracker version and CPU model they were written on. A restored
guest keeps idle vsock connections that are not woken until its keepalive
notices, seconds later.

## Decision

### Free memory goes back to the host

A VM may have a virtio balloon, with free page reporting and statistics.
Free page reporting is what keeps snapshots small: the guest hands freed
memory back, and Firecracker writes it as zeroes.

### Suspend

A suspend writes a full snapshot into the VM's directory, ends its
Firecracker process and punches holes over the pages of zeroes. Asked to
purge, `feos-guest` first drops its caches and compacts its memory
(`PreparePause`), and with free page reporting `feosd` waits until the VMM
stops shrinking.

### Restore

- A restore takes two steps. It loads the snapshot with the vCPUs stopped;
  a failure up to there keeps the snapshot and the VM suspended. Then it
  resumes the vCPUs and removes the snapshot: once the guest ran, its disks
  have moved on, and the snapshot would roll it back against them.
- After a resume, `feosd` pings the guest once and pushes the time and the
  network again (ADR 0007). It does not wait for the guest's uplink.
- A boot of a suspended VM discards its snapshot. That is the way out when
  a snapshot cannot be restored.

### A snapshot pins what it was written with

A snapshot records the Firecracker version, the CPU model and the host's
kernel it was written on, and is not loaded elsewhere. Nodes keep the
Firecracker versions their snapshots need (ADR 0009). The guest's memory
holds the `feos-guest` it was suspended with, which a restore brings back
as it was (ADR 0007).

## Alternatives considered

- **Waiting for the uplink after a resume**, as the sandbox manager first
  did. The resume would hang until the guest's keepalive fires.
- **Diff snapshots on top of a base** now. Nothing takes repeated
  snapshots of one boot yet, and dirty page tracking costs on every write.
  They come with hibernation, if it needs them.

## Consequences

- Pods have no huge pages: Firecracker restores hugetlbfs memory only
  through a userfaultfd handler, and backs neither an initramfs nor a
  balloon with it.
- A suspended pod's disks must stay as they are until it is restored.
- Where `/var/lib/feos` is a tmpfs, as under `feos-host-init`, a snapshot
  stays in memory: suspending frees only what was free in the guest, and a
  restored VM keeps the snapshot's pages mapped besides its own until it
  stops. Suspend saves memory where `/var/lib/feos` has storage below it,
  as on a distribution (ADR 0009).

## Open questions

- **Hibernation and restore on another node.** Restoring from a
  userfaultfd handler, which also restores a snapshot from object storage,
  comes with the disk daemon. Open is whether a suspended pod is exported
  and imported through `feos.pod.v1`, or whether `feosd` points the disk
  daemon at a storage location it owns.
- **Suspend tiers**: when a suspended pod stays on its node and when it
  goes to object storage (ADR 0001).
