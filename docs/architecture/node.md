The Node
========

What runs on a node and how. The decisions behind it:
[ADR 0003](../adr/0003-programs.md) (programs),
[ADR 0005](../adr/0005-firecracker.md) (Firecracker),
[ADR 0008](../adr/0008-suspend-and-restore.md) (suspend and restore),
[ADR 0009](../adr/0009-node-os-and-packages.md) (operating system and
packages).

Today the node agent is `feos` built with the `host` feature; it becomes
`feosd`. Parts that are decided but not built are marked as such.

## Processes

- **`feos`** (to become `feosd`): the API, the VM service, the container
  and image services. As PID 1 it also sets the machine up; as an ordinary
  process it skips that and becomes the subreaper of its children. The
  setup is to move to `feos-host-init`.
- **`feos --task-service`**: the container runtime (libcontainer) in a
  process of its own, started and restarted by the main process. Its pid is
  in `/run/feos/task-service.pid`.
- **The jailer and Firecracker**, one process per running VM.

## Machine setup as PID 1

When `feos` is PID 1 (to become `feos-host-init`), it moves its root off
the initramfs, mounts `/proc`, `/sys`, `/dev`, `/dev/pts`, `/run`, a
tmpfs at `/var/lib/feos` and the cgroup filesystem, configures hugepages
and SR-IOV VFs, configures the
network from router advertisements and DHCPv6, and keeps the clock with
NTP from the servers DHCPv6 names. It takes Ctrl-Alt-Del over: a SIGINT
makes it sync and reboot.

On a distribution all of this is the operating system's
([ADR 0009](../adr/0009-node-os-and-packages.md)). Today `feos` still runs
its NTP worker when it is not PID 1, and then syncs with `pool.ntp.org`;
that goes with the split.

## Directories

| Path | Holds |
|---|---|
| `/var/lib/feos/vms.db` | the VM records (SQLite) |
| `/var/lib/feos/vms/firecracker/<id>/` | the VM's jail: `jail.json` and the chroot `root/` |
| `/var/lib/feos/images/` | pulled images, a `metadata.json` each |
| `/var/lib/feos/containers/` | the host's containers, a `container.json` each |
| `/var/lib/feos/upgrade/` | a downloaded binary before an upgrade |
| `/run/feos/` | the API sockets, the task service's pid, the runtime state of containers |

`/var/lib/feos` is a tmpfs when FeOS is the node's operating system and a
directory on disk on a distribution.

## A VM

A VM's directory, `/var/lib/feos/vms/firecracker/<id>/root/`, is the
chroot its Firecracker runs in:

| File | What it is |
|---|---|
| `config.json` | the Firecracker configuration, written at creation |
| `fc.sock` | Firecracker's API socket |
| `v.sock` | the host end of the guest's vsock device; `v.sock_10001` is where the guest's uplink arrives |
| `console.in` | a FIFO, the serial console's input (Firecracker's stdin) |
| `serial.log` | the serial console's output since the last boot (Firecracker's stdout) |
| `vmm.log` | Firecracker's own log |
| `firecracker.pid`, `firecracker`, `dev/` | the jailer's |
| `vmlinuz`, `initramfs`, `rootfs.img`, `<drive>.img` | the image's files and the data disks, linked in |
| `snapshot/` | the snapshot of a suspended VM: `vmstate` and `memory` |

`jail.json`, beside the chroot, records what a start links in and what a
deletion undoes.

### Lifecycle

- **Create** pulls the VM's image (an OCI image with `vmlinuz`, optionally
  an initramfs and a root disk) and writes `config.json`. No process
  starts.
- **Start** runs the jailer, which starts Firecracker, which boots the VM at
  once. The process ends when the guest reboots or shuts down: exit code 0
  makes the VM stopped, anything else crashed.
- **Shutdown** sends Ctrl-Alt-Del and waits for the process to end.
- **Pause and resume** stop and continue the vCPUs.
- **Suspend** writes a snapshot, ends the process and punches holes over
  the pages of zeroes. **Resume** of a suspended VM loads the snapshot with
  the vCPUs stopped, then resumes them and removes the snapshot.
- **Delete** stops the VM, removes what `jail.json` records, its TAP
  devices and its directory.

### Jail, user and cgroup

Firecracker runs in the jailer: chrooted into the VM's directory, in a
mount namespace of its own, as a user of the VM's own, which owns the
VM's disks and TAP devices. The VM service creates TAP devices that do not
exist and deletes them with the VM.

Every VM has a cgroup of its own, `/sys/fs/cgroup/vms/<id>`, with
`cpu.weight` 100 per vCPU and no caps. Decided, not built: the cgroup
moves below the cgroup `feosd` is given, so that systemd can delegate it.

### The guest

The VM service listens on `v.sock_10001` from the VM's creation to its
deletion and again after a restart of the service. When the guest attaches
(after a boot, or when its call was lost) and after every resume, the VM
service pushes the wall time and, if the VM has one, its network
configuration through the guest's API. After a resume it pings the guest
once instead of waiting for the uplink. `VmInfo.guest` reports the guest;
a `GuestAttachedEvent` follows each attach.

### Balloon

A VM may have a virtio balloon (`MemoryConfig.balloon`), with free page
reporting and statistics. `UpdateBalloon` and `GetBalloon` set and read
it. With `purge_guest`, a suspend first asks the guest to drop its caches
and compact its memory, and waits until the VMM stops shrinking.

## Firecracker

`feos` finds `jailer` and `firecracker` in its `PATH`. The tests download
Firecracker v1.17.0, pinned by sha256. Decided, not built: packaged
versions side by side in `/usr/lib/feos/firecracker/<version>/`, of which
each VM uses the one it was booted with
([ADR 0009](../adr/0009-node-os-and-packages.md)).
