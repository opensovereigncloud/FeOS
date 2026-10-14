# ADR 0005: Firecracker is the VMM, in its jailer

**Status:** Accepted · **Date:** 2026-10-13

## Context

FeOS's VM service ran VMs with cloud-hypervisor, behind a `Hypervisor`
trait. The pods of ADR 0001 need what Firecracker ships: diff snapshots of
guest memory, restore through an external page-fault handler, the jailer
and rate limiters. Keeping both VMMs would mean two backends of which one
is used and the other goes stale. What cloud-hypervisor offers beyond
Firecracker (PCI passthrough, firmware boot of arbitrary disk images,
hotplug, live migration) has no user here (ADR 0001).

The sandbox system's manager ran Firecracker as root without the jailer.
`feosd` runs as root, too.

## Decision

### Firecracker only

Firecracker is FeOS's only VMM. The cloud-hypervisor adapter goes; the
`Hypervisor` trait stays as the seam where another VMM would plug in. FeOS
as it was before stays on the `release-0.5` branch, and 0.6.0 starts the
new line.

- **A VM is a Firecracker process per boot.** Firecracker can neither
  reboot a guest nor power it off; its process ends when the guest
  reboots, and starting the VM again starts a new process.
- **Devices are fixed at creation.** Firecracker cannot attach disks or
  network devices to a running VM. `feos.vm.v1` loses `AttachDisk`,
  `DetachDisk`, `AttachNic`, `DetachNic`, the VFIO backends, `max_vcpus`
  and `ignition`.
- **Shutting a VM down is Ctrl-Alt-Del.** The guest needs the i8042
  keyboard driver and has to reboot on Ctrl-Alt-Del; `feos-guest` does.
- **FeOS speaks Firecracker's API itself**: a handful of JSON requests over
  HTTP/1.1 on a Unix socket, without an HTTP client library.

### Every VM in its jailer

- Firecracker runs in the jailer: in a chroot, the VM's directory, in a
  mount namespace of its own, as a user of the VM's own. What it opens by
  path is linked into the chroot; the VM's user owns its disks and TAP
  devices.
- Every VM has a cgroup of its own, `vms/<id>`, below the cgroup `feosd` is
  given: the root under `feos-host-init`, the cgroup systemd delegates to
  `feosd.service` on a distribution (ADR 0009). VMs outlive a restart of
  `feosd` either way.
- VMs share the CPU in proportion to their vCPUs (`cpu.weight` 100 per
  vCPU), as their vCPU threads would without a cgroup each. The cgroup
  accounts the VM's memory and caps neither.

## Alternatives considered

- **Keeping cloud-hypervisor next to Firecracker.** It would keep PCI
  passthrough and firmware boot, for a use case without a user, at the
  price of a second backend to test and maintain.
- **Keeping the hotplug RPCs** and emulating them with placeholder drives
  that are swapped with `PATCH /drives`. Nothing needs it yet; it can come
  without changing the API.
- **Firecracker as root without the jailer**, as the sandbox manager ran
  it. A VMM escape would own the node.
- **A memory cap per VM** (`memory.max` of the guest's memory and an
  allowance). A suspend is charged to the VM: with its snapshot in a tmpfs,
  which the kernel cannot reclaim without swap, the cgroup of a VM with
  256 MiB reached 519 MiB. A cap would kill the VM in the middle of a
  suspend, or when its guest writes to a sparse disk.

## Consequences

- No PCI passthrough (GPUs, NVMe and SR-IOV VFs into VMs), no firmware
  boot, so no operating systems other than Linux, no hotplug, no live
  migration, no huge pages.
- The microVM layer hands TAP devices to the microVM's user by changing
  their owner.
- Caps per VM, for VMs that overcommit the node, need a way to ask for
  them, storage below `/var/lib/feos` for a memory cap, and a kernel with
  `CONFIG_CFS_BANDWIDTH` for a CPU cap (`cpu.max`), which FeOS's kernel
  config leaves out.
