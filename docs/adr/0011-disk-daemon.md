# ADR 0011: The disk daemon

**Status:** Proposed · **Date:** 2026-10-14

## Context

A pod has no disk: what its containers write is gone when it stops, and a
suspended pod is bound to the node that holds its snapshot (ADR 0008).
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
  zstd is the format in the object store. The cache is a budget of
  reconstructible bytes, enforced by diskd. The top is the only data that
  cannot be fetched again, and is never evicted.
- Commit order: chunks, then the layer's manifest, then the pod's head
  (`head.json`: its chain and memory); upward to the object store in the
  same order. A crash leaves garbage, never a manifest without its
  chunks. A sweeper deletes what the object store holds that nothing
  reaches; the caller names what is live, and nothing younger than a
  grace is touched.

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
meanwhile.

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

## Consequences

- diskd is node-critical: while it is down, every pod's disk I/O waits.
  It needs a restart policy and a watchdog.
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

## Open questions

- How `feosd` uses diskd: when a pod gets a disk, its size, the keys of its
  heads, and whether hibernation and restore on another node go through
  `feos.pod.v1` (ADR 0008).
- Where baking happens: in a tenant's pod, or in a pod of its own with a
  neutral configuration, so that no tenant's state lands in a shared layer.
- How long I/O may wait for a recovering diskd before its pod is given up.
- The host's page cache in front of the devices: keep it, or drop what was
  served.
- Whether the chunk pool stays global, which is dedup across tenants.
