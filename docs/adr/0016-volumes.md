# ADR 0016: Persistent volumes

**Status:** Proposed · **Date:** 2026-10-16

## Context

ADR 0001 leaves persistent volumes open for CaaS: a service's database
has to keep its files when its pod is deleted and made anew, for an
upgrade of its image, a change of its resources or a move. Every pod
has a disk of its own from diskd (ADR 0011), and that disk lives and
dies with its pod.

Firecracker attaches drives at boot only: it has no hotplug (ADR 0005),
and a snapshot restores the drives it was written with. It answers the
guest's `VIRTIO_BLK_T_GET_ID` with the device number and inode of the
file it opened, cut to 20 bytes, not with the drive's ID: a guest
cannot tell its drives apart by what the host named them.

The pods' disks start from a blank ext4 filesystem built with the guest
(ADR 0011), so that nothing on a node runs mkfs. A volume has a size of
its own, and a blank filesystem per size is not a thing to ship.

## Decision

### A volume is a resource of its own

- A volume has an ID, which the caller chooses, so that a create that is
  delivered twice finds it, a size, and labels. It is created, listed,
  read and deleted through `feos.volume.v1.VolumeService`, beside the
  pods, as security groups are (ADR 0012): a resource that a pod's spec
  names by ID, with a life of its own.
- A volume is in one of three states: **available**, attached to no pod;
  **attached** to a pod on the node, in whatever state the pod is; or
  **checkpointed**, its data in object storage alone, as the volume of a
  hibernated pod, or after that pod was deleted. A checkpointed volume
  comes back from its checkpoint at the next boot of a pod that has it.
- Deleting a pod does not delete its volumes: they are available again,
  or stay checkpointed. A volume is deleted only while it is attached to
  no pod.
- Its size is fixed at its creation: from 64 MiB to 16 TiB, in whole
  MiB.

### A pod's spec names its volumes

- `PodSpec.volumes` names the volumes of the pod, each by ID and under a
  name of the pod's own, at most eight; a container mounts a volume by
  that name at a path, read-only if it says so.
- The volumes are fixed at the pod's creation: the microVM gets a
  virtio-blk drive per volume, after its disk, in the order of the
  spec, and a snapshot is restored onto the same drives.
- **ReadWriteOnce.** A volume is attached to at most one pod: creating a
  pod claims its volumes, all or none, and fails with
  `FAILED_PRECONDITION` while one of them is attached to another pod or
  not on the node. The pod holds them until it is deleted. Volumes that
  pods share, over virtio-fs or NFS, are not part of this.

### Each volume is a disk of diskd's

- A volume is a disk of its own in diskd, with its own chain: an empty
  L0 that every volume shares, one chunk of zeroes, and a layer of the
  volume's own; the disk is as large as the volume and thin, so it costs
  what is written to it. It is made at the volume's first boot.
- It follows its pod's microVM as the pod's disk does: started for a
  boot, a resume and a restore, stopped when the microVM ends, by a
  stop, a suspend, a crash or the guest. Deleting the pod leaves it on
  the node, stopped.

### The guest formats a new volume, and only then

- A volume's filesystem is ext4, made in the guest with `mke2fs`: a
  static one from e2fsprogs, built with musl and shipped in the guest's
  initramfs, not in the containers' toolbox. It formats with lazy inode
  tables and journal and without discards, and the guest mounts with
  `noinit_itable`: the disk reads zeroes where nothing was written, so
  `mke2fs` writes a few MiB, and the kernel zeroes nothing later. The
  filesystem's UUID is the volume's ID.
- This is the one exception to "no shelling out to userspace tools":
  `mke2fs` in the guest, nothing else, nowhere else. An ext4 writer of
  our own would be a second implementation of the format, in PID 1.
- **The host decides that a volume is new.** `feosd` keeps whether a
  volume has its filesystem; until the guest confirms that it made it,
  it has not, and every boot until then asks the guest to format. A
  mount that fails never leads to a format.
- **The guest refuses to destroy a filesystem.** Asked to format, it
  formats a disk that holds no filesystem, or one that was never
  mounted: what an interrupted `mke2fs` leaves. A filesystem that was
  mounted (ext4 whose superblock is valid and has a mount time or a
  mount count), or another filesystem's signature, is not formatted. If
  that filesystem is the volume's own, by its UUID, the guest's
  confirmation was lost after the volume had been used: the guest mounts
  it as it is, and reports it as formatted. Anything else fails the
  mount.

### The guest tells its drives by their order

- The host pushes the guest the pod's volumes after it attached and
  before the containers start: each one's name, ID, size, drive and
  whether to format it. The guest orders its virtio disks by the MMIO
  address of their device, which Firecracker hands out in the order of
  the drives, falls back to the order in which the kernel found them,
  and takes the drive's disk.
- It checks that the disk has the volume's size, and that a filesystem
  on it has the volume's ID as its UUID. A drive taken for another
  fails the mount, and at worst formats a disk that was blank.
- Volumes are mounted in the guest's memory filesystem, outside the
  pod's disk, at a directory per name; a container's mount of a volume
  is a bind mount of that directory, read-only where it says so, without
  devices and setuid. The guest refuses a container's mount of a volume
  that is not mounted: it would write into memory.

### Volumes go with their pod into object storage

- A hibernation (ADR 0008) seals and uploads each attached volume with
  the pod: its chain, its record as `feosd` keeps it, and its head, each
  volume's before the pod's. The pod's head names the volumes' heads,
  and diskd writes it only once they are in the bucket; a sweep that
  keeps the pod's head keeps its volumes'. The volumes are checkpointed
  then, and their disks leave the node with the pod's.
- A checkpointed volume of a pod that hibernates again, without having
  booted since, keeps its head.
- A restore takes the volumes from the checkpoint, here or on a node that
  has never seen them, before anything of the pod changes: a node
  records a volume it does not have from the checkpoint, and takes the
  checkpoint's state over a volume of the pod, or over a checkpointed
  volume that no pod holds. It refuses, with `FAILED_PRECONDITION`, when
  a volume of the checkpoint is attached to another pod on the node, or
  is available there with data that the checkpoint may not have, and
  when a volume's head is missing. A restore with memory finds the
  volumes mounted in the guest; a boot mounts them anew.

## Alternatives considered

- **Volumes as RPCs of `feos.pod.v1`.** A volume outlives its pods, and
  the control plane manages it apart from them; a package of its own
  grows, with sizes, snapshots and moves, without the pods' API.
- **The drive's serial number** (`VIRTIO_BLK_T_GET_ID`) to tell drives
  apart: Firecracker answers with the opened file's device number and
  inode, cut to 20 bytes, which changes with every device node the host
  makes; the host could predict it, from what Firecracker does today.
- **A label or UUID alone**: a new volume has neither, and two new
  volumes look the same.
- **mkfs on the node**, by `feosd` or diskd on the ublk device: a
  filesystem the host's tools and kernel make for a guest kernel that
  may know other features, and the host mounting tenants' data.
- **A blank filesystem per size**, built with the guest as the pods'
  disks' is: one per size, and every size another L0.
- **Formatting whenever no filesystem mounts**: a disk that is broken,
  or a drive taken for another, would be formatted over.
- **Refusing to mount a volume's own filesystem that the host believes
  new**: the pod would fail for good after a lost confirmation, with no
  way for the host to learn better.
- **Volumes that pods share** (ReadWriteMany), over virtio-fs or NFS:
  a server per volume, and locking across microVMs, for which nothing
  asks yet.
- **Hotplugging volumes**: Firecracker has none, and a snapshot would
  have to record the drives that a restore attaches.

## Consequences

- `feos.volume.v1` is a new package of the node's API, and `feosctl`
  gets volume commands. `PodSpec` gains volumes, `ContainerSpec` volume
  mounts; the guest's container API gains volume mounts by addition,
  and `feos.guest.v1.GuestService` starts with `MountVolumes`.
- The guest's initramfs carries `mke2fs`, 0.5 MiB, and building the
  guest needs `musl-gcc` and the kernel's headers.
- A pod with volumes needs a guest that mounts them: a node whose newest
  guest predates them boots such a pod into a failure.
- Volumes count against a node's disk as the pods' disks do. A volume
  attached to a hibernated pod holds nothing on the node.
- The control plane places a pod with volumes where its volumes are, or
  hibernates the pod to move them. A free volume does not move between
  nodes on its own.
- Two restores of one checkpoint are two pods with one past, and two
  copies of its volumes (ADR 0008): ReadWriteOnce holds on a node; across
  nodes, the control plane keeps it.
- The pods' disks keep their blank L0: their chunks are shared, and no
  pod waits for a mkfs. With `mke2fs` in the guest, a disk size per pod
  (ADR 0011) could format a disk of another size instead.

## Open questions

- **Growing a volume**: diskd's device, Firecracker's drive (`PATCH
  /drives`) and the filesystem online (`EXT4_IOC_RESIZE_FS`), which
  writes 1.6 % of what it adds, zeroed inode tables, into the top.
- **Ephemeral volumes**, as Kubernetes's `emptyDir`: a directory on the
  pod's disk, bind-mounted like a volume, shared by the pod's
  containers and gone with the pod. Cheap; it waits for sidecars
  (ADR 0004), its first user.
- **Moving a free volume** between nodes, without a pod: a checkpoint of
  the volume alone.
- **Snapshots and clones** of a volume, which diskd's layers would make
  cheap.
- **Quotas**: how much of a node's disk its volumes may take.
