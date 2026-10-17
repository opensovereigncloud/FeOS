# ADR 0011: The disk daemon

**Status:** Proposed · **Date:** 2026-10-15

## Context

Pods had no disk: what their containers wrote was gone when they stopped,
and a suspended pod was bound to the node that held its snapshot
(ADR 0008).
ADR 0001 asks for suspends that cost nothing on the node and for resumes
on other nodes, so a pod's disk and memory have to be able to live in an
object store, and come back from it faster than they could be downloaded
whole. Pulling an image in every new guest is the slow part of a pod's
start: seconds with a warm store, minutes with a cold one, once per pod
and node even when the same image ran next door a minute ago.

Firecracker attaches a raw file or a block device as a drive, nothing
else, and restores memory from a file or through a userfaultfd handler.
The sandbox system built a disk daemon for this, diskd, next to its own
guest; it comes into FeOS here.

## Decision

### One daemon per node owns the disks

`diskd` is a Rust daemon of its own on every node, run as root next to
`feosd`. It owns the block devices behind pods' disks, the node's chunk
cache and the S3 client, and serves a gRPC API (`feos.disk.v1`) on a unix
socket. `feosd` is its client; `diskctl` is the one for development. A
pod's disk outlives the Firecracker process that uses it, and diskd
outlives `feosd`'s restarts and its own.

### Disks are ublk devices

A pod's disk is `/dev/ublkb<N>`, a device of the kernel's userspace block
driver (ublk), served through `libublk`, the library of ublk's
maintainer. A ublk device survives its daemon: with user recovery, a
diskd that crashes or is upgraded leaves the devices in place, their I/O
waits, and the next diskd takes them over from its state file. The pods
see latency, not errors. Each device has one queue and one thread that
owns everything its I/O touches; nothing on the request path takes a
lock (`clippy.toml` denies `Mutex` and `RwLock` there). Firecracker
attaches the device with `cache_type` `Writeback`, so that a guest's
flush reaches diskd and not only the host's page cache.

### A disk is a chain of layers on a pool of chunks

- A disk is a chain of sealed layers and a mutable top. The chain has a
  fixed shape: shared layers (a blank or base `L0`, and a baked image
  layer on it) and at most one layer of the pod's own. A seal merges the
  top into the pod's layer (flatten on seal), so a chain never grows.
- Layers are cut into 64 KiB chunks, content-addressed with blake3, stored
  once per node and once in the object store, whatever pods share them.
  A layer's manifest is a binary table: per chunk data, explicitly zero, or
  absent (inherited from below).
- The top is one sparse file per pod. Writes go there and nowhere else;
  a discard writes zeroes, it never punches a hole, as a hole means "read
  the layer below". A seal finds the changes with `SEEK_DATA` and
  `SEEK_HOLE` and compares hashes, so the write path stays a plain write.
- The node's cache holds raw chunks, so that reads need no decompression;
  zstd is the format in the object store. The budget, enforced by diskd,
  holds chunks that can be fetched again. The top cannot be, and is never
  evicted. A layer of a disk that is not uploaded, and a disk's own layer
  that no head names yet (`unheaded/<disk>`: sealed for a hibernation
  that was abandoned or failed), are pinned outside the budget: the node
  has their only durable copy.
- Commit order: chunks, then the layer's manifest, then the pod's head
  (`head.json`: its chain and memory); upward to the object store in the
  same order. A crash leaves garbage, never a manifest without its
  chunks. A sweeper deletes what the object store holds that nothing
  reaches; the caller names what is live, and nothing younger than a
  grace is touched.
- A marker says a chunk or a layer is in the object store, and a push
  skips what has one. The markers that a push wrote for a disk or a
  memory layer that no head names yet are trusted for 12 hours, then
  what they mark is uploaded again: nothing in the store reaches it, so
  a sweep deletes it once it is older than its grace. A sweep's grace
  must therefore exceed 12 hours plus the longest hibernation.

### Memory goes the same way

A memory dump is sealed as a flat layer of 256 KiB chunks, so that a
checkpoint uploads only the chunks that changed. A restore does not
download it first: diskd serves Firecracker's userfaultfd backend from the
same cache and object store, and the microVM runs while its memory
arrives.

### Baking images

A pod that pulled an image can be sealed live, with the guest's
filesystem frozen, into a shared layer on `L0`. Pods of that image start
from it without a pull. The seal fails if the device was written to
meanwhile. `feosd` does not bake yet.

### Every pod has a disk

- Every pod has one disk from diskd, and there is no other kind: no pod
  without a disk, no disk of another kind. A disk is a capability of every
  pod, like its memory, so the pod's spec has no field for it. `feosd`
  reaches diskd on its socket; while diskd cannot be reached, pods do not
  come up. A pod's volumes (ADR 0016) are disks of diskd's too, beside
  its disk, with lives of their own.
- A disk starts from a blank data volume, an ext4 filesystem that comes
  with the guest, which mounts it at `/var/lib/feos`: the guest's images,
  its containers and what they write live there. The volume is built with
  the guest, the same bytes from every build, and ships compressed: its
  8 GiB hold 6 MiB. `feosd` has diskd import the newest guest's volume
  once, as the `L0` of every pod's disk; diskd unpacks it sparse.
- Its size is every pod's disk's size, 8 GiB. A disk is thin: it costs
  what the pod writes, on the node and in the object store.
- A pod's chain is that `L0` and at most one layer of its own. A head
  records which layer is the pod's own, so that a restore elsewhere keeps
  the chain's shape: the next seal replaces the pod's layer instead of
  stacking a new one on it.
- Firecracker gets the disk as a device node of its own in its jail, made
  from the link diskd keeps per pod at every boot and restore: a device's
  number changes when it is started anew, and the snapshot holds the
  jailed path, which is the same on every node.

### A disk lives as long as its pod

- A boot starts the pod's disk: the one the node has, else the one of the
  pod's checkpoint (ADR 0008), else a new one on `L0`. Containers the disk
  holds from the boot before are adopted: their writable layers stay,
  their processes start anew.
- When the microVM ends, by a stop, a suspend, a crash or the guest, the
  disk stops and keeps its top. A started device locks 128 MiB of
  buffers, which a pod that runs nothing should not hold.
- A crash keeps the disk: a start boots on it, as after a stop.
- A delete removes the disk and its top. While diskd cannot remove it, the
  delete fails and can be repeated, so that no top is left behind.
- A hibernation stops, seals and uploads the disk, and removes it from the
  node once the head is up (ADR 0008).

### The calls feosd makes can be repeated

`feosd` restarts, and its calls get lost. `OpenDisk` returns a device that
runs already, and `Seal` of a disk without a top returns the layer it
sealed; the seal points the device's row at the new layer before it
removes the top, so that a crash in between loses nothing. Each call
that changes a disk holds the disk's lock until its work on the node
ends, whether or not its caller is still there. A seal whose caller went
away before it committed the new layer stops at its next chunk, and the
disk opens again as it was; one that committed it ends first, and the
disk opens again over the sealed layer. Its upload stops where it is;
what was uploaded stays, and the next push skips it.
The head of a checkpoint is diskd's to write, last (`PutHead`): from the
device's row, only once every layer it names and the vmstate are in the
bucket, and never over another head. `GetHead` reads one. `feosd` needs
no head format of its own, and the object store stays diskd's alone.

### diskd beside feosd

diskd is a process of its own, packaged beside `feosd` (ADR 0009): the
package `diskd` brings `/usr/sbin/diskd`, `diskctl`, `diskd.service`,
`/etc/default/diskd` with the object store's credentials, root's alone,
and loads the ublk driver at every boot. The unit is ready once diskd
serves, its devices recovered; `feosd` wants it and starts after it. A
restart of diskd stops no unit and no pod. Under `feos-host-init`, diskd
runs beside `feosd` and is started again when it ends. The host kernel
builds in XFS, for the state directory's filesystem.

## Alternatives considered

- **FUSE** serving a file: a daemon that dies takes every pod's disk with
  it, its operations do not fit a block device, and each request crosses
  the VFS.
- **NBD**: a socket protocol whose reconnect is no match for ublk's
  recovery.
- **Our own binding of ublk's `uring_cmd` interface**: a second
  implementation of an interface that is still moving, in a daemon every
  pod depends on.
- **A compressed local cache**: decompression on every read.
- **dm-thin, qcow2, ZFS or btrfs snapshots**: bound to one node's storage
  where the disk has to move between nodes, with dedup by block, not by
  content. dm-thin stays the fallback for a node without ublk.
- **A reflink copy of a local image per pod**: cheap, but a second disk
  path that cannot be sealed the same way.
- **One daemon per pod**: the cache and the S3 client belong to the node.
- **`feosctl disk`** instead of `diskctl`: diskd's API is internal to the
  node, like the microVM layer, and two of `diskctl`'s commands talk to
  the kernel through libublk, which `feosctl` should not link.
- **A disk in the pod's spec, optional**: two kinds of pods, and every
  capability, a stop, a crash, a hibernation, decided twice. The sandbox
  system kept pods with plain disk files beside diskd's and dropped them
  for that.
- **mkfs on the node**, by `feosd` or the guest: a tool to shell out to, or
  an ext4 writer of our own. A blank volume built with the guest is the
  same file everywhere. The guest runs `mke2fs` for volumes now
  (ADR 0016); the pods' disks keep the blank volume, whose chunks every
  pod shares, and no pod waits for a mkfs.
- **The blank volume uncompressed in the package**: dpkg writes holes as
  zeroes, 8 GiB per guest version.
- **A disk size per pod**, by a blank volume per size or by growing the
  filesystem online in the guest (`EXT4_IOC_RESIZE_FS`). Possible later;
  the kernel zeroes the inode tables of what it grows, 1.6 % of it,
  written into the top. With `mke2fs` in the guest (ADR 0016), a disk of
  the pod's size could also start empty and be formatted there.
- **Devices that stay started** while their pod is stopped or suspended,
  as the sandbox system's did: 128 MiB locked per pod that runs nothing.
- **feosd linking the chunk store** instead of a daemon of its own: a
  restart or an upgrade of `feosd` would take every pod's disk with it.

## Consequences

- diskd is node-critical: while it is down, every pod's disk I/O waits.
  It needs a restart policy and a watchdog.
- A node's cache can outgrow its budget by the pinned layers: the own
  layers of pods whose hibernation was abandoned or failed, until their
  next head. They are bytes the node held before the seal, as tops.
- The host kernel needs ublk (Linux 6.1 for recovery) and userfaultfd.
  Requests are copied once on 6.12; ublk's zero-copy needs 6.15 or 6.16.
- One thread per device. Hundreds of pods mean hundreds of mostly idle
  threads until libublk serves several devices from one.
- libublk comes from a pinned git revision; its async API is not
  released.
- The state directory belongs on a filesystem of its own, preferably XFS,
  with room for the cache budget in raw chunks.
- A node that tiers to an object store needs its credentials (S3_BUCKET,
  AWS_*).
- `feosd` needs diskd: without it, no pod comes up. Tests that run `feosd`
  in a container run diskd beside it, and give the container the node's
  `/dev`, where ublk's devices appear.
- What a pod writes survives a stop, a crash and a restart of `feosd` or
  diskd; a pod's containers come back with their writable layers.
- Every guest version ships its blank volume; a guest whose volume changes
  starts new disks on another `L0`.

## Open questions

- Where baking happens: in a tenant's pod, or in a pod of its own with a
  neutral configuration, so that no tenant's state lands in a shared layer.
- A disk size per pod: a filesystem the guest grows, or one it formats
  with `mke2fs`, as it does volumes (ADR 0016).
- Where diskd keeps its state on a node whose operating system is FeOS: in
  the tmpfs, every pod's top is in memory, and diskd has no object store's
  credentials there.
- How long I/O may wait for a recovering diskd before its pod is given up.
- The host's page cache in front of the devices: keep it, or drop what was
  served.
- Whether the chunk pool stays global, which is dedup across tenants.
