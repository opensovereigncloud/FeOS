The Node
========

What runs on a node and how. The decisions behind it:
[ADR 0003](../adr/0003-programs.md) (programs),
[ADR 0005](../adr/0005-firecracker.md) (Firecracker),
[ADR 0008](../adr/0008-suspend-and-restore.md) (suspend and restore),
[ADR 0009](../adr/0009-node-os-and-packages.md) (operating system and
packages).

Parts that are decided but not built are marked as such.

## Processes

- **`feos-host-init`**: PID 1 when FeOS is the node's operating system. It
  sets the machine up, starts `feosd` and starts it again when it ends.
- **`feosd`**: the node agent: the API, the pod and microVM services, the container and
  image services. It runs under systemd or `feos-host-init`, never as
  PID 1.
- **`feosd --task-service`**: the container runtime (libcontainer) in a
  process of its own, started and restarted by `feosd`. Its pid is in
  `/run/feos/task-service.pid`.
- **The jailer and Firecracker**, one process per running pod.

## Machine setup: feos-host-init

`feos-host-init` moves its root off the initramfs, mounts `/proc`, `/sys`,
`/dev`, `/dev/pts`, `/run`, a tmpfs at `/var/lib/feos` and the cgroup
filesystem, configures SR-IOV VFs, configures the network
from router advertisements and DHCPv6, and keeps the clock with NTP from
the servers DHCPv6 names. It takes Ctrl-Alt-Del over (a SIGINT makes it
sync and reboot), reboots on `SIGRTMIN+5` and powers off on `SIGRTMIN+4`,
reaps orphans, and runs `/usr/sbin/feosd --self-upgrade --tcp [::]:1337`,
which it starts again a second after it ends. It serves no API and logs
to `/dev/kmsg`.

On a distribution all of this is the operating system's
([ADR 0009](../adr/0009-node-os-and-packages.md)).

## feosd

`feosd [--tcp ADDRESS] [--self-upgrade]` sets nothing of the machine up.

- It serves the API on `/run/feos/api.sock` and, with `--tcp`, on TCP,
  without authentication ([api.md](api.md)). If a group `feos` exists, it
  owns the socket, mode 0660: its members can do on the node what root
  can.
- Once its listeners are bound, it sends `READY=1` to `$NOTIFY_SOCKET` if
  systemd set one.
- `HostService.Reboot` and `Shutdown` send PID 1 `SIGRTMIN+5` and
  `SIGRTMIN+4`. `UpgradeFeosBinary` replaces the binary and runs it with
  the same pid only with `--self-upgrade`; without, the call fails with
  `FAILED_PRECONDITION`.
- It is the subreaper of its children. Pods and the task service outlive
  it: a `feosd` that starts again takes the running pods over and keeps the
  running task service.

## Directories

| Path | Holds |
|---|---|
| `/var/lib/feos/pods/<id>.pb` | a pod: its spec, its state and why (a `feos.pod.v1.PodInfo`) |
| `/var/lib/feos/vms.db` | the microVM records (SQLite) |
| `/var/lib/feos/vms/firecracker/<id>/` | a microVM's jail: `jail.json` and the chroot `root/` |
| `/var/lib/feos/images/` | pulled images, a `metadata.json` each |
| `/var/lib/feos/containers/` | the host's containers, a `container.json` each |
| `/var/lib/feos/upgrade/` | a downloaded binary before an upgrade |
| `/run/feos/` | the API sockets, the task service's pid, the runtime state of containers |
| `/usr/lib/feos/firecracker/<version>/` | Firecracker and its jailer, a version each, side by side |
| `/usr/lib/feos/guest/<version>/` | the guest's `vmlinuz` and `initramfs`, a version each, side by side |

`/var/lib/feos` is a tmpfs when FeOS is the node's operating system and a
directory on disk on a distribution.

## A pod

A pod ([ADR 0004](../adr/0004-pods.md)) is a microVM
with the pod's ID, booted from the newest installed guest, and the
containers its spec names, which the guest runs with the names as their
IDs. Its spec gives vCPUs and memory, the containers (image, command,
environment) and a network: a TAP device the caller made, and the guest's
address, gateway and resolvers. The microVM has a balloon with free page
reporting.

- **Create** records the pod as `PENDING` and brings it up in the
  background: create and start the microVM, wait for the guest to attach
  (60 s at most), then create (which pulls) and start the containers, in
  order. The pod is then `RUNNING`, or `FAILED` with the reason. Each step
  looks at what is there first, so a bring-up that a restart of `feosd`
  cut off is taken up again where it ended. A create with the ID of a pod
  that exists returns that pod if the spec is the same, and fails with
  `ALREADY_EXISTS` if not.
- **Stop** ends a bring-up, shuts the microVM down, or removes it if it
  does not go or is not running; **Start** brings a stopped or failed pod
  up anew. A pod has no disk yet: what its containers wrote is gone.
- **Suspend** and **Resume** suspend and resume the microVM, with
  `purge_guest`; the containers come back as they were.
- **Delete** ends a bring-up and removes the microVM and the record.
- A running pod whose microVM crashes is `FAILED`; one whose microVM stops
  is `STOPPED`.

What changes a pod runs under a lock of the pod's. `GetPod` asks the guest
for the containers' states while the pod runs.

## A pod's microVM

The microVM layer under the pods (`crates/services/microvm-service`,
internal `feos.microvm.v1`) runs the microVMs; FeOS runs no VMs of their
own. A microVM's directory, `/var/lib/feos/vms/firecracker/<id>/root/`, is
the chroot its Firecracker runs in:

| File | What it is |
|---|---|
| `config.json` | the Firecracker configuration, written at creation |
| `fc.sock` | Firecracker's API socket |
| `v.sock` | the host end of the guest's vsock device; `v.sock_10001` is where the guest's uplink arrives |
| `console.in` | a FIFO, the serial console's input (Firecracker's stdin) |
| `serial.log` | the serial console's output since the last boot (Firecracker's stdout) |
| `vmm.log` | Firecracker's own log |
| `firecracker.pid`, `firecracker`, `dev/` | the jailer's |
| `vmlinuz`, `initramfs` | the installed guest's files, linked in |
| `snapshot/` | the snapshot of a suspended microVM: `vmstate` and `memory` |

`jail.json`, beside the chroot, records what a start links in and what a
deletion undoes.

### Lifecycle

- **Create** takes the newest installed guest and keeps that version: its
  jail links that version's files. It writes `config.json`; no process
  starts.
- **Start** runs the jailer, which starts Firecracker, which boots the
  microVM at once. The process ends when the guest reboots or shuts down:
  exit code 0 makes the microVM stopped, anything else crashed.
- **Shutdown** sends Ctrl-Alt-Del and waits for the process to end.
- **Suspend** writes a snapshot, ends the process and punches holes over
  the pages of zeroes. **Resume** loads the snapshot with the vCPUs
  stopped, then resumes them and removes the snapshot.
- **Delete** stops the microVM, removes what `jail.json` records, its TAP
  devices and its directory.

The layer records a change of state after the call that made it returns;
the pod service waits for it. A microVM has no data disks (the disk daemon
brings volumes) and no huge pages, which Firecracker backs neither an
initramfs nor a balloon with.

### Jail, user and cgroup

Firecracker runs in the jailer: chrooted into the microVM's directory, in
a mount namespace of its own, as a user of the microVM's own, which owns
its TAP devices. The layer creates TAP devices that do not exist and
deletes them with the microVM.

Every microVM has a cgroup of its own, `vms/<id>` below the cgroup `feosd` is
given, with `cpu.weight` 100 per vCPU and no caps: `/sys/fs/cgroup/vms/<id>`
under `feos-host-init`, `/sys/fs/cgroup/system.slice/feosd.service/vms/<id>`
under systemd. A cgroup whose children use controllers may hold no
processes, so `feosd` moves into the leaf `feosd` of its cgroup, where
systemd starts it already with `DelegateSubgroup=feosd`. `feosd` creates
the microVM's cgroup and enables `cpu` and `memory` in its own cgroup and
in `vms/`, never above; the jailer only moves Firecracker into it.

### The guest

The layer listens on `v.sock_10001` from the microVM's creation to its
deletion and again after a restart of `feosd`. When the guest attaches
(after a boot, or when its call was lost) and after every resume, it
pushes the wall time and, if the pod has an address, its network
configuration through the guest's API. After a resume it pings the guest
once instead of waiting for the uplink.

### Balloon

A pod's microVM has a virtio balloon with free page reporting: the guest
hands the memory it frees back. A suspend first asks the guest to drop
its caches and compact its memory, and waits until the VMM stops
shrinking.

## Firecracker

Firecracker versions are installed side by side in
`/usr/lib/feos/firecracker/<version>/`, as the packages lay them out
([ADR 0009](../adr/0009-node-os-and-packages.md)). A boot runs the newest
version that has `firecracker` and `jailer`, or, where none is installed,
the ones in `PATH`; `jail.json` records which. A restore runs what the
snapshot's boot ran, and fails with `FAILED_PRECONDITION` if that is not
installed any more. The tests download Firecracker v1.17.0, pinned by
sha256.

## Packages

On Debian 13 and Garden Linux, `feosd` and what it runs come as `.deb`
packages from https://apt.fecode.dev
([ADR 0009](../adr/0009-node-os-and-packages.md)):

| Package | Contents |
|---|---|
| `feosd` | `/usr/sbin/feosd`, `feosd.service`, the group `feos` (sysusers), `/etc/default/feosd`, `/etc/apt/apt.conf.d/50feosd` |
| `feosctl` | `/usr/bin/feosctl` |
| `feos-firecracker-<version>` | `/usr/lib/feos/firecracker/<version>/{firecracker,jailer}` |
| `feos-guest-<version>` | `/usr/lib/feos/guest/<version>/{vmlinuz,initramfs}` |
| `feos-firecracker`, `feos-guest` | nothing; they depend on the newest versioned package |

`feosd` depends on `feos-firecracker` and `feos-guest`, which depend on the
newest versioned packages, as `linux-image-amd64` does on the newest
kernel: an upgrade installs a new Firecracker or guest beside the old
ones, which the pods booted with them keep using. `50feosd` keeps every
version from `apt autoremove`; a version nothing uses is removed with
`apt remove`.

`feosd.service` is `Type=notify`, with `Delegate=yes`,
`DelegateSubgroup=feosd` and `KillMode=process`: a restart or an upgrade
stops `feosd` alone, and the next one takes the pods over.
`StateDirectory=feos` makes `/var/lib/feos` a directory on disk.
`FEOSD_ARGS` in `/etc/default/feosd` passes options, such as `--tcp`.
Installing the package enables the unit the first time and starts it, or
restarts it after an upgrade, whenever it is enabled; removing the package
stops the unit; purging it disables the unit. `/var/lib/feos` stays.

The repository has one suite, `stable`, with the component `main`, and is
signed by its own key:

    curl -fsSL https://apt.fecode.dev/feos.gpg -o /usr/share/keyrings/feos.gpg
    echo 'deb [signed-by=/usr/share/keyrings/feos.gpg] https://apt.fecode.dev stable main' \
      > /etc/apt/sources.list.d/feos.list
    apt-get update && apt-get install feosd feosctl

`make publish-apt` builds the packages of `HEAD` as
`<version>~dev.<UTC time>.<commit>`, below the release they lead to, and
`hack/packages/publish-apt` uploads them to the bucket behind the
repository, keeping what was published before.
