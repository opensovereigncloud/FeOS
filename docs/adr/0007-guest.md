# ADR 0007: feos-guest, the pod's PID 1, and its API

**Status:** Accepted · **Date:** 2026-10-13

## Context

The sandbox system's guest was a Rust PID 1 in a Firecracker microVM that
ran an agent's containers. Its host pushed the configuration over vsock,
and the manager used a gRPC API of eleven services. FeOS's own PID 1 took
its configuration from the network (DHCPv6, NTP) and served its API on
TCP. The pods of ADR 0004 run `feos-guest` (ADR 0003); this ADR records
what it does and which API carries what.

## Decision

### Transport and configuration

- The API listens on vsock port 10000 and on `/run/feos/api.sock`, never
  on TCP.
- Once its listeners are up, `feos-guest` dials the host (vsock CID 2,
  port 10001) and holds `feos.lifecycle.v1.LifecycleService.Attach` open:
  readiness and liveness. The socket a call comes in on names the VM to
  `feosd`; the guest never names itself.
- **Configuration is pushed, not discovered.** When a guest attaches,
  after a boot or when its call was lost, and after every resume, `feosd`
  pushes the wall time and the pod's network. The guest runs no DHCP and
  no NTP.
- The data volume, the virtio disk that is not the root, is mounted at
  `/var/lib/feos`.
- Kubernetes and SSH credentials stay on the host. The guest pipes their
  connections to vsock ports 10002 and 10003.

### API

The sandbox system's services map onto FeOS's APIs. Where FeOS had a
service for the same subject, it grows; what is new gets a package of its
own, and so does what only a guest does: the host pushing its
configuration into it, `feos.guest.v1`.

| Sandbox system | FeOS |
|---|---|
| `AgentService.Ping` | `feos.host.v1.HostService.Ping` (tick, uptime, boot id) |
| `AgentService.GetAgentInfo` | `HostService.GetVersionInfo` and `Ping` |
| `AgentService.GetMemoryInfo` | `HostService.GetMemoryBreakdown` |
| `AgentService.PreparePause` | `feos.guest.v1.GuestService.PreparePause` |
| `FsService.Freeze`, `Thaw` | `GuestService.FreezeFilesystem`, `ThawFilesystem` |
| `TimeService.SetTime`, `GetTime` | `GuestService.SetTime`, `GetTime` |
| `NetworkService.SetNetworkConfig`, `GetNetworkConfig` | `GuestService.SetNetworkConfig`, `GetNetworkConfig` |
| `ImageService.Pull`, `ListImages` | `feos.image.v1.ImageService.PullImage` + `WatchImageStatus`, `ListImages` |
| `ContainerService.*`, `Exec` | `feos.container.v1.ContainerService.*`, `Exec` |
| `ToolService`, `WatchService` | `feos.tool.v1`, `feos.watch.v1` |
| `PortForwardService`, `KubeService`, `SshService` | `feos.portforward.v1`, `feos.kube.v1`, `feos.ssh.v1` |
| `LifecycleService` | `feos.lifecycle.v1` |

`HostService` is the same in `feosd` and `feos-guest`: what the machine is
and its power. In `feos-guest`, shutting down reboots, which ends the VM;
powering off would halt the guest and leave Firecracker running.

### The guest's API is internal and only grows

The guest's API is a contract between `feosd` and `feos-guest` (ADR 0004).
A suspended pod keeps the `feos-guest` it was suspended with, so a newer
`feosd` talks to older guests: the guest's APIs, `feos.lifecycle.v1`
included, change only by addition within v1. No field is renumbered or
removed, and no RPC changes its meaning.

### Runtime

- **libcontainer in a process of its own.** Containers run with youki's
  libcontainer, linked in, not with the youki binary, in the task service
  (`feos-guest --task-service`), which PID 1 supervises. A runtime failure
  does not take down PID 1.
- **Container records are files**, a `container.json` in the container's
  directory, not rows in sqlite. A record lives and dies with its
  container's directory and survives a reboot with the writable layer that
  adoption takes over. The guest has no SQLite.
- **One HTTP client.** Image pulls use oci-client on reqwest, with rustls
  and ring; so do webfetch and the binary upgrade. oci-client checks the
  digest of every blob it pulls.
- `feos-guest` takes Ctrl-Alt-Del over: the kernel sends it a SIGINT
  instead of rebooting at once, and it syncs and reboots.

### What the port of the manager has to change

Against the sandbox system's API, besides the package names:

- **Pulls are asynchronous**: `PullImage` returns an image UUID, and
  `WatchImageStatus` reports when it is ready. Images are immutable and
  shared; a request marked `exclusive` gets an image of its own, which a VM
  needs because it writes to its image's disk.
- **`CreateContainer` returns once the container is created**, or fails
  with the reason. It pulls the image if needed. `adopt` works as before.
- **Container IDs** are names of 1 to 64 letters, digits, `-`, `_` and
  `.`, which is what the sandbox system called the container name. FeOS
  generates a UUID when a client gives none.
- `DeleteContainer` kills a running container. Container state is an enum
  (`CREATED`, `RUNNING`, `STOPPED`), not a string.
- A container's hostname is its ID, not "sandbox".

## Alternatives considered

- **Keeping the sandbox system's API as it is**, under the FeOS name.
  FeOS would carry two APIs for containers and images.
- **The guest-only RPCs in `HostService`.** `feosd` would serve them, too:
  `FreezeFilesystem` on a node would freeze the host's root,
  `SetNetworkConfig` rewrite its network.
- **DHCP and NTP in the guest**, as FeOS's PID 1 did. A restored guest's
  clock and network are wrong until the next lease or sync; a push after
  every resume fixes them at once, and the guest needs no network to be
  configured.
- **libcontainer in PID 1**, as the sandbox system ran it. Simpler, but a
  runtime failure, or a fork from a multithreaded process gone wrong,
  would hit PID 1.
- **sqlite for container records.** It would cost the guest 18 crates and
  SQLite's C code, and a database that has to stay consistent with the
  containers' directories across freezes, snapshots and reboots.

## Consequences

- `feosd` is the only client of the guest's API.
- Not ported: the `webfetch` command-line mode. Exec still has neither a
  terminal nor stdin.
