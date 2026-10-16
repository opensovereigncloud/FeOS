# ADR 0008: Suspend and restore

**Status:** Accepted; hibernation and restore elsewhere Proposed ·
**Date:** 2026-10-15

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

### Hibernation: a pod in object storage (Proposed)

Every pod has a disk from the disk daemon (ADR 0011), which hibernation
builds on.

- `feos.pod.v1` gets `HibernatePod` and `RestorePod`, and a state for a
  hibernated pod: a record on the node with its checkpoint, and nothing
  else of it there. When a pod hibernates is the control plane's policy,
  not `feosd`'s, as is where it comes back. Without a control plane, the
  API and `feosctl` drive it.
- A checkpoint is the pod: its disk's layers, its memory as a flat layer,
  Firecracker's vmstate, its spec, and the head that names them, last.
  Its objects are under `snapshots/<pod>/<epoch>/`; the epoch is the
  seconds since 1970, or one more than the last one's, so that no head is
  written twice. All of a node's traffic with the object store runs
  through the disk daemon.
- A hibernation runs under the pod's lock, in this order, on a node whose
  disk daemon has an object store; elsewhere it is refused before the pod
  changes. The pod is suspended, as above, if it runs (the guest syncs,
  Firecracker flushes the disk); its disk stops, is sealed and uploaded;
  its memory is sealed and uploaded; its vmstate and its spec are put
  beside them; the head goes last. Then the pod is hibernated, and what it
  held on the node goes: its microVM and snapshot, its disk, its addresses
  and its TAP device. Until the head is up the pod stays suspended on the
  node, and a hibernation that fails leaves it so; what it uploaded is
  garbage for the sweep. A stopped or failed pod hibernates its disk
  alone.
- The pod's volumes (ADR 0016) go with it: each is sealed and uploaded
  beside the disk, with a head of its own before the pod's, which names
  them. They leave the node with the pod's disk.
- The guest's filesystem is not frozen. A restore with memory brings the
  memory and the disk back together; a boot on the disk alone sees it as
  after a power cut at the snapshot, synced a moment before, which ext4's
  journal keeps whole.
- A resume that comes while a pod hibernates waits for the pod's lock and
  finds it hibernated: it restores it, from the node's cache. `ResumePod`
  restores a hibernated pod like `RestorePod` does with the pod's own
  checkpoint.

### Restore (Proposed)

- `RestorePod` takes a checkpoint's head. A node that does not have the
  pod records it from the checkpoint's spec; one that has a disk of the
  pod drops it for the checkpoint's. The pod gets the node's addresses,
  which the guest is pushed after the restore. Its volumes come from the
  checkpoint too; a restore whose volumes another pod on the node holds
  is refused before anything changes (ADR 0016).
- Whether the memory is restored is decided before anything starts, from
  what the snapshot pins: the Firecracker that wrote it must be installed,
  on the same CPU model and kernel. Then the disk daemon serves the memory
  to Firecracker's userfaultfd backend from the memory layer, and the
  guest runs before its memory is downloaded. Otherwise, or when asked,
  the pod boots anew on the checkpoint's disk and adopts its containers:
  what they wrote stays, what ran in them is gone. A restore of the
  memory that fails before the guest ran falls back to that boot; the
  disk is as the checkpoint has it.
- A pod keeps its checkpoint in its record while it runs from it: its
  disk reads layers of it lazily, so a sweep of the object store keeps the
  heads of all pods' checkpoints.
- Stopping a hibernated pod drops its memory; a start boots it on the
  checkpoint's disk. Deleting it forgets it; its objects go with a sweep.
- A crashed pod boots anew on its disk at its next start, like a
  checkpoint without memory.

## Alternatives considered

- **Waiting for the uplink after a resume**, as the sandbox manager first
  did. The resume would hang until the guest's keepalive fires.
- **Diff snapshots on top of a base** now. Nothing takes repeated
  snapshots of one boot yet, and dirty page tracking costs on every write.
  Hibernation does not need them: a memory layer is uploaded in chunks
  the object store does not have, which is what an increment is.
- **Hibernation as a flag of `SuspendPod`**, or tiers `feosd` decides:
  when a pod leaves its node is a policy, and policies are the control
  plane's (ADR 0001).
- **Exporting a pod through `feos.pod.v1`** as a stream of bytes, for the
  caller to keep: every byte would cross `feosd` and the control plane,
  and a restore could not read the memory lazily.
- **Uploading outside the pod's lock**, so that a pod stays resumable
  while its checkpoint uploads, as the sandbox system did: a resume during
  the upload resumes on the node, and the checkpoint stays but does not
  count. The lock is simpler to reason about; a resume pays for it by
  waiting for the upload, after which it restores from chunks the node
  still has.
- **Trying the snapshot and booting anew if the load fails**, whatever the
  pins say: a snapshot loaded on the wrong host can corrupt the guest
  instead of failing. The pins decide; a load that fails all the same
  falls back.
- **Freezing the guest's filesystem** before the snapshot: every restore
  with memory would have to thaw it, and a guest that is not thawed stops
  writing.
- **Downloading the memory before the restore**, a file Firecracker loads:
  the guest waits for all of it. With the memory served lazily, it waits
  for what it touches.

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
- A pod restored with its memory depends on the disk daemon until its
  next suspend or stop: the daemon serves pages the guest has not touched
  yet, and a userfaultfd does not survive the daemon. A restart of the
  daemon freezes such guests until they are stopped; a disk survives it.
  The first suspend after such a restore reads every page through the
  daemon.
- Checkpoints pile up in the object store until a sweep with the heads
  of all pods removes what none of them reaches; the control plane knows
  them all.
- Two restores of one checkpoint are two pods with one past. The control
  plane places a pod on one node at a time.
- A node that restores a pod needs the pod's security groups.

## Open questions

- **Suspend tiers**: when a suspended pod stays on its node and when it
  goes to object storage (ADR 0001).
- **Detaching the memory**: the disk daemon could install all of a
  restored guest's memory right after the restore and let go of it, so
  that the guest no longer depends on the daemon.
- **Compaction before a hibernation**: compacting the guest's memory
  moves pages, and every moved page is a chunk to upload again.
