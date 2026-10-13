# ADR 0004: The VM half of the sandbox manager in the VM service

**Status:** Accepted · **Date:** 2026-10-13

## Context

The sandbox manager does two jobs. Its VM half starts Firecracker,
snapshots and restores VMs, serves the guest's lifecycle uplink and pushes
the clock and the network into the guest. Its sandbox half keeps the
sandboxes' records, uploads hibernated sandboxes, places them, and binds
Kubernetes and SSH. ADR 0003 made Firecracker FeOS's VMM and left the VM
half to come. This ADR records how the VM service takes it over, so that a
sandbox service on top of FeOS only speaks `feos.vm.v1` to the host and the
guest's API to the guest.

## Decision

### The guest's uplink and what is pushed

- The VM service listens for every VM on `<vsock_path>_10001`, where
  Firecracker forwards the guest's connections to vsock port 10001, from
  the VM's creation to its deletion and again after a restart of the
  service. It serves `feos.lifecycle.v1.LifecycleService`. The socket a
  call came in on names the VM; the guest never does.
- When a guest attaches, after a boot or when its call was lost, the VM
  service pushes the wall time and, if the VM has one, its network
  configuration (`VmConfig.guest_network`) through the guest's API.
  `GuestAttachedEvent` follows; after a cold boot, that is when the guest
  serves. `VmInfo.guest` reports the guest.
- After a resume the VM service pings the guest once and pushes again. It
  does not wait for the uplink: a call that sat idle before a snapshot is
  not woken in the restored guest until its keepalive notices, seconds
  later.

### Balloon

`MemoryConfig.balloon` adds a virtio balloon at boot, with free page
reporting and statistics if asked. `UpdateBalloon` and `GetBalloon` set and
read it. Free page reporting is what keeps snapshots small: the guest hands
freed memory back, and Firecracker writes it as zeroes.

### Suspend and restore

- `SuspendVm` writes a full snapshot into the VM's directory, ends its
  Firecracker process and punches holes over the pages of zeroes. The VM
  is `VM_STATE_SUSPENDED`. With `purge_guest`, a FeOS guest first drops
  its caches and compacts its memory (`PreparePause`), and with free page
  reporting the VM service waits until the VMM stops shrinking.
- `ResumeVm` restores a suspended VM in two steps. It loads the snapshot
  with the vCPUs stopped; a failure up to there keeps the snapshot and the
  VM suspended. Then it resumes the vCPUs and removes the snapshot: once
  the guest ran, its disks have moved on, and the snapshot would roll it
  back against them.
- A snapshot records the Firecracker version, the CPU model and the kernel
  it was written on and is not loaded elsewhere. `StartVm` boots a
  suspended VM afresh and discards its snapshot, which is the way out when
  a snapshot cannot be restored.

### The jailer

Firecracker runs in the jailer: in a chroot, the VM's directory
(`/var/lib/feos/vms/firecracker/<id>/root`), in a mount namespace of its
own, as a user of the VM's own. What it opens by path is linked into the
chroot. The VM's user owns the VM's disks and TAP devices. The VM service
creates TAP devices that do not exist and deletes them with the VM.
`jail.json`, beside the chroot, records what a start links in and what a
deletion undoes.

## Alternatives considered

- **Waiting for the uplink after a resume**, as the sandbox manager first
  did. Idle vsock connections are not woken in a restored guest; the
  resume would hang until the guest's keepalive fires.
- **Diff snapshots on top of a base.** Nothing takes repeated snapshots of
  one boot yet, and dirty page tracking costs on every write. They come
  with the sandbox service, if hibernation needs them.
- **Running Firecracker as root without the jailer**, as the sandbox
  manager does. FeOS is the host's PID 1; a VMM escape would own the host.

## Consequences

- A sandbox service needs no Firecracker of its own: it creates, boots,
  suspends and restores VMs through `feos.vm.v1`, and talks to the guest
  through `VmInfo.vsock_path`.
- VMs with huge pages cannot be suspended: Firecracker restores hugetlbfs
  memory only through a userfaultfd handler. Restoring from a userfaultfd
  handler, which also restores a snapshot from S3 on another host, comes
  with diskd.
- A suspended VM's disks must stay as they are until it is restored.
- The VM service hands disk files and TAP devices to a user of the VM's
  own by changing their owner.
- Firecracker joins no cgroup of its own yet; CPU and memory limits per VM
  come with the jailer's cgroup options.
