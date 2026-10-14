# ADR 0009: The node's operating system and packages

**Status:** Proposed · **Date:** 2026-10-14

## Context

`feosd` (ADR 0003) runs on a node under one of two operating systems:

- FeOS itself: `feos-host-init` as PID 1, booted from a unified kernel
  image (UKI), as FeOS ran before 0.6.0;
- a general-purpose distribution with systemd: Debian 13, or Garden Linux,
  which builds on Debian.

The sandbox system ran its manager as a privileged DaemonSet in the host's
PID namespace and copied Firecracker and the guest onto the host, so that a
rollout would not take the binaries from under running VMs. Whatever the
operating system, `feosd` needs:

- pods that outlive a restart and an upgrade of `feosd` (ADR 0001);
- the Firecracker version a snapshot was written with (ADR 0008): an
  upgrade that replaces Firecracker leaves suspended pods that cannot be
  restored;
- no port open to receive work (ADR 0006).

## Decision

### Debian and Garden Linux first

Nodes run Debian 13 or Garden Linux first. FeOS as the node's operating
system stays a goal, and runs the same `feosd`: nothing in `feosd` depends
on which of them it runs under.

### Debian packages

The binaries are static musl (ADR 0002) and depend on no library of the
distribution, so one `.deb` serves Debian 13 and Garden Linux. nfpm builds
the packages, because Firecracker and the guest are no cargo crates. They
are published in a signed apt repository of our own.

| Package | Contents |
|---|---|
| `feosd` | `/usr/sbin/feosd`, `feosd.service`, tmpfiles and sysusers |
| `feosctl` | `/usr/bin/feosctl` |
| `feos-firecracker-<version>` | Firecracker and the jailer, in `/usr/lib/feos/firecracker/<version>/` |
| `feos-guest-<version>` | the guest's kernel and initramfs, in `/usr/lib/feos/guest/<version>/` |

Firecracker and the guest are installed side by side, as kernel packages
are: an upgrade adds the new version and leaves the old ones. `feosd`
boots new pods with the newest versions it finds, records in each pod
what it booted with, and reports which versions its pods and snapshots
still use. A version nothing uses may be removed. The disk daemon and the
per-pod proxies follow the same scheme when they arrive.

Garden Linux takes the packages when its image is built, in a feature that
lists them. Its nodes are not changed with apt at runtime.

### feosd under systemd

- `feosd.service` is `Type=notify`. `feosd` sends `READY=1` to
  `$NOTIFY_SOCKET` once its listeners are bound: one datagram, no
  libsystemd.
- `Delegate=yes` and `KillMode=process`. `feosd` moves itself into a leaf
  of its cgroup and puts VMs into `vms/<id>` below it (ADR 0005). A
  restart or an upgrade stops `feosd` alone; the pods keep running, and
  the new `feosd` takes them over, as containerd's shims outlive
  containerd.
- `feosd` sets nothing of the machine up. Network, time and SR-IOV VFs
  are the operating system's: systemd-networkd, timesyncd or
  chrony, the kernel command line.
- `/var/lib/feos` is the unit's state directory, on disk. That is what
  makes suspend free memory (ADR 0008).
- `HostService.Reboot` and `Shutdown` send PID 1 `SIGRTMIN+5` and
  `SIGRTMIN+4`, which systemd answers with `reboot.target` and
  `poweroff.target`. No D-Bus, no `systemctl`.
- `feosd` replaces its own binary (`UpgradeFeosBinary`) only when it is
  started with `--self-upgrade`, which `feos-host-init` passes. On a
  distribution apt owns the binary, and the RPC fails with
  `FAILED_PRECONDITION`.

### FeOS as the node's operating system

The UKI's initramfs carries `feos-host-init` as `/init`, `feosd`, and
Firecracker and the guest, laid out as the packages lay them out.
`feos-host-init` sets the machine up (ADR 0003), mounts a tmpfs at
`/var/lib/feos`, starts `feosd` with `--self-upgrade` and starts it again
when it ends. It answers the same signals as systemd, so `feosd` reboots
and powers off the node the same way under both.

## Alternatives considered

- **A DaemonSet**, as the sandbox system ran its manager. Kubernetes would
  own `feosd`'s life: a VMM started in a pod lives in its cgroup and PID
  namespace and dies with it, unless the pod runs privileged in the host's
  PID namespace and moves its VMs into cgroups outside its own. A systemd
  unit gets there with two settings.
- **One package with Firecracker and the guest in it.** An upgrade would
  replace the Firecracker that suspended pods need.
- **cargo-deb.** It packages one crate; Firecracker and the guest are none.
- **systemd-sysext images**, for nodes with a read-only `/usr`. They may
  follow; apt and the Garden Linux image build take `.deb`.
- **`feosd` setting the node up**, as FeOS did as PID 1. On a distribution
  it would contend with its network and time services.

## Consequences

- The release builds the packages and publishes the apt repository; it
  still builds the UKI.
- A snapshot is restored only on a node that has its Firecracker and guest
  packages. Placement learns which a node has from its telemetry
  (ADR 0006).
