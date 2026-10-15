The Disk Daemon
===============

`diskd` serves pods' disks as block devices over a chunk store that it
tiers to an object store, and restores pods' memory from the same chunks.
Every pod has a disk from it. The decision behind it:
[ADR 0011](../adr/0011-disk-daemon.md).

## Processes

- **`diskd`** (`crates/diskd`): one per node, as root, beside `feosd`.
  It needs the kernel's ublk driver (`/dev/ublk-control`,
  `CONFIG_BLK_DEV_UBLK`) and, for memory, userfaultfd
  (`CONFIG_USERFAULTFD`). Each device has a thread of its own, and a
  thread that fetches its missing chunks.
- **`feosd`** is its client, on `--diskd-socket`
  (`/var/lib/feos/disk/diskd.sock`); see [below](#the-pods-disks).
- **`diskctl`** (`crates/diskctl`): its client for development, the lab
  scripts, tests and operators. It prints JSON.

`diskd [--state-dir DIR] [--socket PATH] [--budget-bytes N]
[--s3-concurrency N] [--mem-readahead N] [--s3-prefix P]`. The state
directory defaults to `/var/lib/feos/disk`, the socket to
`<state-dir>/diskd.sock` (mode 0660, as `feosd`'s), the cache budget to
8 GiB. Logs are JSON when stdout is not a terminal; `RUST_LOG` filters
them.

The object store is S3 or compatible, configured from the environment:
`S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`AWS_ENDPOINT_URL` (path-style) and `AWS_REGION` (default `us-east-1`).
Without them diskd runs from its local store only: what is not cached
there fails to read.

## A pod's disk

A disk is a chain of layers, L0 first, and a mutable top:

```
L0       a base image (ImportBase), or a blank data volume
L1       a baked image layer on L0 (Bake), optional
S        the pod's own layer, from its last seal, optional
top      a sparse file: what the pod wrote since
```

- `CreateDisk(pod, chain)` starts a device over the chain and a fresh
  top, and links `<state>/live/<pod>/disk` to `/dev/ublkb<N>`. A device
  has 4 KiB physical and 512 B logical blocks, one queue of depth 128,
  1 MiB requests, a volatile cache (flushes reach diskd) and discard.
- Reads go top first, then down the chain: a chunk is read from the
  cache, fetched from the object store into it when missing, or zero.
  Writes go to the top. Discards and write-zeroes write zeroes into the
  top.
- `ImportBase(path)` seals a raw image into an L0; an image whose name
  ends in `.zst` is unpacked first, sparse, into `staging/`.
- `StopDisk` stops the device and keeps the top. `OpenDisk(pod)` starts it
  again over the same chain and top, or returns it if it runs;
  `OpenDisk(pod, head_key)` starts it on a node that has no disk of the
  pod, from a head in the object store, with a fresh top.
- `Seal(pod, upload)`, on a stopped device, merges the top into the pod's
  layer S, points the device's row at it and then removes the top; with
  `upload`, it pushes the chain. A disk without a top is sealed already,
  and the seal returns its newest layer. `Bake(pod)` seals the frozen top
  of a running device into a layer on L0 and changes nothing about the
  pod.
- `PutHead(pod, epoch, memory)` uploads the head of a checkpoint of a
  sealed, stopped disk: its chain, how much of it is shared, its size,
  and the memory the caller sealed and put beside it. It refuses a head
  whose layers or vmstate are not in the bucket, and one that exists:
  heads are never overwritten. `GetHead(key)` reads one.
- `RemoveDevice` forgets a stopped pod.

When diskd ends, its devices stay. The next diskd recovers every device in
`diskd.state` before it serves: I/O that waited goes on.

## Memory

`SealFile(path)` seals a memory dump into a flat layer of 256 KiB chunks.
`OpenMemory(pod, layer, socket)` binds a socket for Firecracker's UFFD
memory backend (`PUT /snapshot/load` with `backend_type` `Uffd`) and serves
the guest's page faults from the layer; pages the guest reported free come
back as zero pages. `CloseMemory` detaches it.

## The pods' disks

Every pod has one disk (`crates/services/pod-service/src/disk.rs`):

- **L0** is the blank data volume of the newest guest installed,
  `/usr/lib/feos/guest/<version>/data.img.zst`: an 8 GiB ext4 filesystem,
  label `feos-data`, made by `make guest-data` (`hack/guest/data/mk-data`)
  with a fixed UUID, hash seed and time, so that every build is the same
  file, and its inode tables and journal zeroed. `feosd` has diskd import
  it the first time a pod needs it after `feosd` started.
- **A boot** of a pod (create, start) opens its disk: `OpenDisk(pod)` if
  diskd has it, else `CreateDisk(pod, [L0])`. The microVM's `data_disk` is the link
  `<state>/live/<pod>/disk`; the microVM layer makes a device node of it,
  `data` in the jail, at every start and restore, and Firecracker attaches
  it as `/data` with `cache_type` `Writeback`. The guest mounts it at
  `/var/lib/feos`. On a boot after which the disk holds the pod's
  containers, `feosd` creates them with `adopt`: their writable layers and
  logs stay.
- **When the microVM ends**, by a stop, a suspend, a crash or the guest,
  `feosd` stops the disk (`StopDisk`) and lets go of memory diskd served
  it (`CloseMemory`). A resume opens the disk before it restores the
  microVM.
- **A delete** stops and removes the disk (`RemoveDevice`), its top with
  it, and fails while diskd cannot.

A pod from before pods had disks gets one at its next boot, in a new
microVM.

## Directories

| Path (in the state directory) | Holds |
|---|---|
| `diskd.sock` | the API |
| `diskd.state` | the device table: pod, ublk device id, chain, size |
| `cache/<hh>/<hash>` | raw chunks, within the budget |
| `staging/` | chunks and manifests before their rename, a compressed image while it is imported |
| `layers/<layer>.manifest` | layer manifests |
| `live/<pod>/top.img`, `live/<pod>/disk` | a pod's top, and the link to its device |
| `snapshots/<pod>/head.json` | a pod's head after its last seal: chain, size |
| `uploaded/` | which chunks and layers are in the object store |
| `zero.bin` | zeroes to serve reads from |

The state directory belongs on a filesystem of its own, preferably XFS.

| Key (in the bucket) | Holds |
|---|---|
| `chunks/<hh>/<hash>.zst` | chunks, zstd |
| `layers/<layer>.manifest` | layer manifests, uploaded after their chunks |
| `snapshots/<pod>/<epoch>/head.json`, `…/vmstate`, `…/pod.pb` | a checkpoint: its head (`PutHead`), last; the caller's own objects (`PutObject`) |

A head (`head.json`) names the disk's chain, L0 first, how many of its
layers are shared (`shared_depth`; the rest, at most one, is the pod's
own and the next seal replaces it), the disk's size and chunk size, the
epoch, and, for a checkpoint with memory, the memory layer, the vmstate's
key and what the snapshot pins: the Firecracker version, the CPU model and
the host's kernel.

`Sweep` deletes what the bucket holds that the live heads and layers it is
given do not reach, and nothing younger than a day.

## Development

    make diskd               # static diskd and diskctl
    make diskd-lab-up        # a lab diskd with its state in target/diskd/state
    make diskd-lab           # the lab tests against it, as root
    make diskd-lab-drill     # SIGKILL diskd under fio; it must recover
    make diskd-lab-down      # LAB_FLAGS=--clean also stops its devices
    make diskd-mkscratch     # an XFS volume for the benchmarks
    make diskd-bench-store   # the chunk store's benchmark

The lab tests are `#[ignore]`d in `cargo test`. Those that need S3 skip
without `S3_BUCKET` and `AWS_*` in the environment, and use a key prefix
of their own.

`make test-pods` runs a diskd beside `feosd` in its container, with the
node's `/dev`, and the pods' integration tests against it: disks across a
stop, a start and a crash.
