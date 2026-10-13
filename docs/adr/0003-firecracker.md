# ADR 0003: Firecracker is the VMM

**Status:** Accepted · **Date:** 2026-10-13

## Context

FeOS's VM service ran VMs with cloud-hypervisor, behind a `Hypervisor`
trait. The system FeOS grows into (ADR 0001) runs its microVMs with
Firecracker, because Firecracker ships what the sandboxes need: diff
snapshots of guest memory, restore through an external page-fault handler,
the jailer and rate limiters. Keeping both VMMs would mean two backends of
which one is used and the other goes stale. What cloud-hypervisor offers
beyond Firecracker (PCI passthrough, firmware boot of arbitrary disk
images, hotplug, live migration) has no user here: IronCore runs
cloud-hypervisor through its own provider on a plain Linux host, not on
FeOS.

## Decision

Firecracker is FeOS's only VMM. The cloud-hypervisor adapter goes; the
`Hypervisor` trait stays as the seam where another VMM would plug in. FeOS
as it was before stays on the `release-0.5` branch, and 0.6.0 starts the new
line.

### A VM is a Firecracker process per boot

- `CreateVm` pulls the VM's image and writes its Firecracker configuration.
  It starts no process.
- `StartVm` starts a Firecracker process that boots the VM at once.
- Firecracker can neither reboot a guest nor power it off: the process ends
  when the guest reboots, which makes the VM stopped (exit code 0) or
  crashed (anything else). Starting it again starts a new process.
- `ShutdownVm` sends Ctrl-Alt-Del and waits for the process to end. The
  guest needs an i8042 keyboard driver and has to reboot on Ctrl-Alt-Del.
- `PauseVm` and `ResumeVm` pause and resume the vCPUs.
- The serial console is Firecracker's stdin and stdout: a FIFO and a log
  file in the VM's directory, which outlive a restart of the service.

### What a VM is made of

- The image is an OCI image with the kernel (`vmlinuz`), and optionally an
  initramfs and a root disk, which becomes `/dev/vda`.
- Data disks and TAP devices are fixed when the VM is created; Firecracker
  cannot attach devices to a running VM.
- Every VM has a vsock device. `VmInfo.vsock_path` is its host end, through
  which a client reaches the guest's ports.

### API and code

- `feos.vm.v1` loses `AttachDisk`, `DetachDisk`, `AttachNic` and
  `DetachNic`, the VFIO backends, `max_vcpus` and `ignition`, which
  cloud-hypervisor passed in SMBIOS OEM strings.
- The Firecracker API is a handful of JSON requests over HTTP/1.1 on a Unix
  socket; FeOS speaks it itself. The cloud-hypervisor client, hyperlocal and
  the hyper client leave the VM service.

### The guest

- The guest kernel has the i8042 keyboard driver.
- PID 1 takes Ctrl-Alt-Del over: the kernel sends it a SIGINT instead of
  rebooting at once, and FeOS syncs and reboots. On bare metal that makes
  Ctrl-Alt-Del a clean reboot.
- In the guest profile, `HostService.Shutdown` reboots, which ends the VM.
  Powering off would halt the guest and leave Firecracker running.

## Alternatives considered

- **Keeping cloud-hypervisor next to Firecracker.** It would keep PCI
  passthrough and firmware boot, for a use case without a user, at the price
  of a second backend to test and maintain.
- **Keeping the hotplug RPCs** and emulating them with placeholder drives
  that are swapped with `PATCH /drives`. Nothing needs it yet; it can come
  without changing the API.

## Consequences

- No PCI passthrough (GPUs, NVMe and SR-IOV VFs into VMs), no firmware
  boot, so no operating systems other than Linux, no hotplug, no live
  migration. FeOS still sets up SR-IOV VFs and hugepages on bare metal;
  hugepages back VM memory when a VM asks for them.
- `make test-vm` runs the VM integration tests against Firecracker, with
  the guest profile as the VM image.
- Firecracker runs as root without the jailer, confined by its seccomp
  filters only. The jailer, snapshots and restore, the balloon, and the
  host end of the guest's lifecycle uplink are still to come. With them, the
  VM half of the sandbox manager (starting Firecracker, snapshots, pushing
  time and network after a boot or resume) moves into the VM service; the
  sandbox half stays a service of its own.
