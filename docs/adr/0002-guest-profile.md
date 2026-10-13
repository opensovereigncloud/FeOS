# ADR 0002: The guest profile and its API

**Status:** Accepted · **Date:** 2026-10-13

## Context

ADR 0001 decided that FeOS evolves into the merged system and that the
sandbox system's functionality is ported into the FeOS crates. Its PID 1
runs in a Firecracker microVM and runs an agent's containers. Its host
pushes the configuration over vsock, and a manager on the host uses a gRPC
API of eleven services. This ADR records how that functionality lands in
FeOS: which API carries what, and how the guest is built.

## Decision

### Profile and transport

`feos` built with `--features guest` (and without `host`) is the guest:

- The API listens on vsock port 10000 and on `/run/feos/api.sock`, never
  on TCP.
- Once its listeners are up, FeOS dials the host (vsock CID 2, port 10001)
  and holds `feos.lifecycle.v1.LifecycleService.Attach` open: readiness
  and liveness.
- The data volume, the virtio disk that is not the root, is mounted at
  `/var/lib/feos`.
- Kubernetes and SSH credentials stay on the host. The guest pipes their
  connections to vsock ports 10002 and 10003.

### API

The sandbox system's services map onto FeOS's APIs. Where FeOS already had
a service, it grows. What is new gets a package of its own.

| Sandbox system | FeOS |
|---|---|
| `AgentService.Ping` | `feos.host.v1.HostService.Ping` (tick, uptime, boot id) |
| `AgentService.GetAgentInfo` | `HostService.GetVersionInfo` and `Ping` |
| `AgentService.GetMemoryInfo`, `PreparePause` | `HostService.GetMemoryBreakdown`, `PreparePause` |
| `FsService.Freeze`, `Thaw` | `HostService.FreezeFilesystem`, `ThawFilesystem` |
| `TimeService.SetTime`, `GetTime` | `HostService.SetTime`, `GetTime` |
| `NetworkService.SetNetworkConfig`, `GetNetworkConfig` | `HostService.SetNetworkConfig`, `GetNetworkConfig` |
| `ImageService.Pull`, `ListImages` | `feos.image.v1.ImageService.PullImage` + `WatchImageStatus`, `ListImages` |
| `ContainerService.*`, `Exec` | `feos.container.v1.ContainerService.*`, `Exec` |
| `ToolService`, `WatchService` | `feos.tool.v1`, `feos.watch.v1` |
| `PortForwardService`, `KubeService`, `SshService` | `feos.portforward.v1`, `feos.kube.v1`, `feos.ssh.v1` |
| `LifecycleService` | `feos.lifecycle.v1` |

What a client of the sandbox system's API has to change, besides the
package names:

- **Pulls are asynchronous**, as they always were in FeOS: `PullImage`
  returns an image UUID, and `WatchImageStatus` reports when it is ready.
  Images are immutable and shared: pulling a reference that is present or
  being pulled returns that image. A request marked `exclusive` gets an
  image of its own; the VM service needs that, because a VM writes to its
  image's disk.
- **`CreateContainer` returns once the container is created**, or fails
  with the reason, instead of returning at once and failing only in the
  log. It pulls the image if needed. `adopt` works as before.
- **Container IDs** are names of 1 to 64 letters, digits, `-`, `_` and
  `.`, which is what the sandbox system called the container name. FeOS
  generates a UUID when a client gives none.
- `DeleteContainer` kills a running container, as it did before. Container
  state is an enum (`CREATED`, `RUNNING`, `STOPPED`), not a string.
- A container's hostname is its ID, not "sandbox".

### Runtime

- Containers run with youki's libcontainer (0.7), linked into FeOS, not
  with the youki binary. The task service that drives it runs in a process of its
  own, `feos --task-service`, which PID 1 supervises. The runtime forks
  from there, and a runtime failure does not take down PID 1. That process
  is the subreaper of its containers.
- One reaper per process owns waitpid(2) (`feos_utils::reaper`).
- Exec joins the container's namespaces from the task service and streams
  through the container service.
- A container's root is an overlay over the image's rootfs. Containers get
  the toolbox (static uutils coreutils) at `/feos-tools` and the share
  directory (`/run/feos/share`) read-only at `/run/feos`, with the pushed
  resolvers and the SSH agent socket.
- **Container records are files.** The container service keeps its
  `ContainerRepository`, but a record is a `container.json` in the
  container's directory on the data root, not a row in sqlite. A record
  lives and dies with its container's directory and survives a reboot with
  the writable layer that adoption takes over. Startup reconciles the
  records: containers the runtime does not know are stopped, and
  interrupted creations are removed. Adoption reads the previous spec from
  the bundle. Images keep a `metadata.json` per image. sqlite remains only
  in the host profile's VM service; the guest has no SQLite.
- **One HTTP client.** Image pulls use oci-client on reqwest, with rustls
  and ring; so do webfetch and the binary upgrade. oci-client checks the
  digest of every blob it pulls.

### Build

- The guest kernel (Linux 7.2.6) starts from Firecracker's microVM config.
- The initramfs carries FeOS and the toolbox.
- Firecracker v1.17.0 is downloaded for tests.
- Every download is pinned by sha256.
- `make test-guest` and `make test-guest-container` boot the guest under
  Firecracker and exercise the API end to end.

## Alternatives considered

- **Keeping the sandbox system's API as it is**, under the FeOS name. The
  manager would have changed less, but FeOS would have carried two APIs for
  containers and images. ADR 0001 decided that FeOS's APIs stay and grow.
- **libcontainer in PID 1**, as the sandbox system ran it. Simpler, but a
  runtime failure, or a fork from a multithreaded process gone wrong, would
  hit PID 1. The process boundary was already in FeOS's design (the task
  service as a shim).
- **Keeping sqlite for container records.** It would have kept the SQL
  behind `ContainerRepository`, but cost the guest 18 crates and SQLite's
  C code, and a database file that has to stay consistent with the
  containers' directories across freezes, snapshots and reboots.

## Consequences

- The host-side manager (Go today) has to move to the `feos.*` APIs. That
  is part of bringing over the host components.
- Not ported: the `webfetch` command-line mode. Exec still has neither a
  terminal nor stdin.
