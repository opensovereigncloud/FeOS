# ADR 0020: On Kubernetes, FeOS runs in a pod per node, and its microVMs end with it

**Status:** Accepted · **Date:** 2026-10-26

## Context

On the Gardener shoot `fecode-dev`, FeOS runs as on a Debian node (ADR
0009): a DaemonSet's init container lays the packages over Garden Linux's
read-only `/usr` as a systemd system extension, and systemd runs `feosd`
and `diskd` beside kubelet. MicroVMs live in `feosd.service`'s cgroup and
outlive a restart of `feosd`.

That fits a node that stays. Gardener's nodes do not: an update of the
operating system or of Kubernetes, and every maintenance window, drains a
worker, deletes its machine and creates a new one under a new name. What
lives on the host goes with the machine anyway, and three gaps show:

- **A replaced node takes its sandboxes with it.** The machine controller
  drains a node by evicting its pods and skips DaemonSet pods; kubelet
  has no graceful node shutdown (`shutdownGracePeriod` 0). Nobody tells
  FeRun, so the sandboxes on the node are not hibernated or moved: their
  microVMs end when the machine is deleted.
- **A new node does not join.** A node joins FeRun with a join token for
  its name, which an operator makes by hand (`deploy/gardener/README.md`).
  A replacement worker has a new name and no token.
- **Kubernetes does not know the microVMs.** Their memory is outside every
  pod, so the scheduler places FeCode, the registry and PostgreSQL on
  memory the microVMs use, and kubelet's eviction does not see them.

And systemd 258 and later start a service's main process in the service's
cgroup before moving it into its subgroup, which the kernel refuses while
the cgroup has controllers on for its children: `feosd.service` does not
start again while microVMs run below it. On Garden Linux (systemd 259)
that left a node unmanaged during the rollout of ADR 0019.

## Decision

On Kubernetes, FeOS is Kubernetes workload, not host services. The
packages for Debian (ADR 0009) and `feos-host-init` stay as they are.

### A pod per node

- A DaemonSet, `feos-node`, runs one pod per FeOS worker, with two
  containers from one image, `diskd` and `feosd`, the same binaries as
  the packages. Nothing is installed on the host: no extension, no unit,
  no file in `/etc`. Configuration comes from a ConfigMap and Secrets.
- The pod is privileged and on the host's network (TAP devices, nftables,
  the mesh's WireGuard device, the resolver's address): `hostNetwork`,
  the host's `/dev` (KVM, TUN, ublk), `hostPath` `/var/lib/feos` for the
  microVMs' directories, snapshots, disks and the node's identity, which
  outlive the pod on the node, and the kernel modules it loads (`ublk_drv`).
- The pod requests the memory and CPU that the node gives microVMs
  (FeRun's capacity of the node), plus feosd's and diskd's own, and has no
  limits. The scheduler keeps that memory free of other pods, and kubelet's
  eviction counts the microVMs against the request. Its priority class is
  `system-node-critical`.

### MicroVMs live in the pod

- `feosd` puts each microVM into a cgroup `vms/<id>` below its container's
  cgroup, as below the root under `feos-host-init` (ADR 0005). The
  microVMs belong to the container: when it ends, they end.
- **A stop is planned** (a rollout, a deletion of the pod, `SIGTERM`):
  `feosd` announces its restart to FeRun, as now, suspends every running
  pod into its directory (ADR 0008), and exits. The next `feosd` on the
  node resumes the pods it suspended for the stop. The pod's
  `terminationGracePeriodSeconds` covers the suspend of the node's
  memory at the disk's speed.
- **A crash is not** (`SIGKILL`, the OOM killer, a panic): its microVMs
  end. FeRun treats the sandboxes as on a lost node: back from their last
  hibernation, or lost. That is the price of microVMs that Kubernetes
  knows and ends with their pod; a restart that keeps them would need them
  outside the pod, where neither Kubernetes nor systemd lets them be.

### Nodes come and go with Kubernetes

A small controller, `feos-k8s`, runs as a Deployment beside ferund and
speaks to the Kubernetes API and to FeRun's admin API:

- **Join.** For a node that runs `feos-node` and has not joined, it makes a
  join token for the node's name, once, and puts it into the Secret the pod
  reads. A replaced worker joins without an operator.
- **Drain.** For every FeOS node it keeps a guard pod, bound to the node,
  under a PodDisruptionBudget that allows no disruption. When the machine
  controller (or `kubectl drain`, or the cluster autoscaler) cordons the
  node, the eviction of the guard is refused and retried; the controller
  drains the node in FeRun (`POST /api/v1/admin/nodes/{node}/drain`):
  FeRun hibernates its sandboxes into the object store and restores them
  on other nodes. Once the node is drained, the controller deletes the
  guard, the drain goes on, and the machine goes. An uncordon undrains it.
- **Removal.** When a Kubernetes node is gone, the controller removes the
  FeOS node from FeRun.

The controller is Go in `go/`, with the standard library only: the
Kubernetes API is JSON over HTTPS with the service account's token.

## Alternatives considered

- **systemd on the host, as now**, with the microVMs' cgroups in a unit of
  their own that runs no process (`feos-vms.service`), which fixes the
  restart under systemd 258. The microVMs would outlive a crash of
  `feosd`, but the node would stay a host the cluster does not know,
  replaced nodes would still lose their sandboxes, and every host change
  needs an installer. The fix stays in the Debian packages.
- **MicroVMs outside the pod** (moved out of the container's cgroup): they
  would outlive a restart of the container, but in a cgroup that neither
  kubelet nor systemd owns, and kubelet would not count their memory.
- **A Kubernetes pod per microVM** (as Kata Containers or KubeVirt do):
  the scheduler would place sandboxes, which is FeRun's job (ADR 0015).
- **Graceful node shutdown** in kubelet: the machine controller drains
  first and deletes the machine after, and DaemonSet pods are not part of
  the drain; nor would a shutdown wait for hibernations of minutes.

## Consequences

- A rollout of `feos-node` suspends and resumes every pod on the node,
  one node at a time: a pause of seconds to minutes per sandbox instead of
  none. A crash of `feosd` ends the node's microVMs.
- A node update or a maintenance window moves sandboxes to other nodes
  before the worker goes; it needs an object store, and room on the other
  workers, or the drain waits until the machine controller's drain timeout.
- A new worker joins on its own.
- The sysext installer, the units on Garden Linux and the `OnDelete`
  update strategy go. Moving a running node over is a drain in FeRun, the
  switch, and an undrain, one node at a time.
