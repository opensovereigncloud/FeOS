The Guest
=========

What runs inside a pod's microVM. The decisions behind it:
[ADR 0007](../adr/0007-guest.md). The guest is `feos-guest`, `crates/feos`
built with the `guest` feature.

## Boot

The guest is a kernel and an initramfs. The kernel starts from
Firecracker's minimal microVM config: virtio-mmio devices, balloon and
vsock, IPv6 and IPv4, no modules. The initramfs holds the guest as `/init` and
a static uutils coreutils, the toolbox for containers. Kernel, Firecracker
and coreutils downloads are pinned by sha256 (`hack/guest/`).

As PID 1 the guest:

1. moves its root off the initramfs and mounts `/proc`, `/sys`, `/dev`,
   `/dev/pts`, `/run`, a tmpfs at `/var/lib/feos` and the cgroup
   filesystem;
2. mounts the data volume, the virtio disk that is not the root, at
   `/var/lib/feos`, if the VM has one;
3. sets `vm.overcommit_memory` to 1, so that software that reserves more
   address space than it touches starts in a small VM;
4. takes Ctrl-Alt-Del over: a SIGINT makes it sync and reboot;
5. starts the task service (`--task-service`) and binds its listeners;
6. dials the host on vsock port 10001 and holds
   `LifecycleService.Attach` open.

It runs no DHCP and no NTP: the host pushes the time and the network when
the guest attaches and after every resume.

## Network

A pod is IPv6 only ([ADR 0010](../adr/0010-pod-network.md)): `feosd`
pushes `eth0` an address of the node's pod prefix as a /128, `fe80::1` as
its gateway, `feosd`'s resolver (`fdfd:7b5:61d9::53`) and no IPv4
address. The guest turns router advertisements and address
autoconfiguration off before the interface goes up, adds the address
without duplicate address detection, since the node is the link's only
other end, and routes everything via `fe80::1` on `eth0`. A push leaves
the interface with exactly its addresses: those of an earlier push go,
and so does a family the push leaves out, such as the IPv4 of a pod from
the first network; the same push again changes nothing. A resume on
another node will push another address.

Names resolve through DNS64: a name with only IPv4 addresses gets IPv6
ones under the NAT64 prefix, which the node translates. A program that
connects to an IPv4 literal, or prefers IPv4 from DNS as busybox's
`wget` does, finds no route.

## Listeners and vsock ports

| Port or path | What |
|---|---|
| vsock 10000, `/run/feos/api.sock` | the guest's API ([api.md](api.md)) |
| vsock 10001 (dialled) | the uplink to the host |
| vsock 10002 (dialled) | Kubernetes connections, piped to the host's proxy |
| vsock 10003 (dialled) | SSH agent connections, piped to the host's agent |
| `/run/feos/image.sock`, `/run/feos/task.sock` | internal |

## Directories

| Path | Holds |
|---|---|
| `/var/lib/feos/images/` | pulled images, a `metadata.json` each |
| `/var/lib/feos/containers/` | containers, a `container.json` and the writable layer each |
| `/var/lib/feos/upgrade/` | a downloaded binary before an upgrade |
| `/run/feos/containers/` | the runtime's state of running containers |
| `/run/feos/share/` | what every container sees read-only at `/run/feos` |
| `/usr/lib/feos-tools/` | the toolbox |

## Containers

- A container's root is an overlay over its image's rootfs, with a
  writable layer in its directory.
- Every container gets the toolbox read-only at `/feos-tools` and the
  share directory read-only at `/run/feos`: the pushed resolvers
  (`/run/feos/resolv.conf`) and, while SSH bindings exist, the SSH agent
  socket (`/run/feos/ssh-agent.sock`).
- A container's hostname is its ID.
- The task service runs containers with libcontainer and is the subreaper
  of their processes. Exec joins a container's namespaces from the task
  service and streams through the container service; it has no terminal
  and no stdin yet.
- **Records are files.** Startup reconciles them: containers the runtime
  does not know are stopped, and interrupted creations are removed. A
  container survives a reboot of the guest with its writable layer, and a
  create with `adopt` takes it over.

## Power

`HostService.Shutdown` reboots, which ends the VM: Firecracker cannot
power a guest off, and a halted guest would leave Firecracker running.
