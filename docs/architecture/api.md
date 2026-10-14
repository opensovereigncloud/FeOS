The API
=======

The `feos.*` gRPC APIs are the contract between the node and the control
plane, and between `feosd` and `feos-guest`. They live in
`proto/feos/<api>/v1/`, in the package `feos.<api>.v1`.

## Packages

| Package | What it is for | Served by today |
|---|---|---|
| `feos.vm.v1` | VMs: create, start, shut down, pause, suspend, resume, balloon, console, events | `feosd` |
| `feos.container.v1` | containers: create, start, stop, delete, exec, logs, events | `feosd`, `feos-guest` |
| `feos.image.v1` | OCI image pulls and the image store | `feosd`, `feos-guest` |
| `feos.host.v1` | the machine: hostname, memory, CPU, network, version, logs, power, upgrade; and, for now, the guest's pushed time and network, freezing and `PreparePause` | `feosd`, `feos-guest` |
| `feos.task.v1` | the container runtime, internal between the main process and the task service | both, internally |
| `feos.lifecycle.v1` | the guest's uplink: `Attach` is held open while the guest runs | `feosd` (server), `feos-guest` (client) |
| `feos.tool.v1` | an agent's file and web tools on a container's files: read, write, edit, ls, rg, fd, webfetch | `feos-guest` |
| `feos.watch.v1` | inotify subscriptions on a container's files | `feos-guest` |
| `feos.portforward.v1` | streams to ports inside the guest | `feos-guest` |
| `feos.kube.v1`, `feos.ssh.v1` | the listeners for Kubernetes and SSH credential offloading | `feos-guest` |

Decided, not built:

- `feos.guest.v1.GuestService` takes `SetTime`, `GetTime`,
  `SetNetworkConfig`, `GetNetworkConfig`, `FreezeFilesystem`,
  `ThawFilesystem` and `PreparePause` out of `HostService`; only
  `feos-guest` serves it ([ADR 0007](../adr/0007-guest.md)).
- `feos.pod.v1.PodService` replaces `feos.vm.v1` for clients, and a pod's
  guest services are reached through `feosd` with the metadata
  `feos-pod-id` ([ADR 0004](../adr/0004-pods.md), proposed).

## Listeners

On a node (`feos` built with `host`, to become `feosd`):

| Listener | Services |
|---|---|
| TCP `[::]:1337` | Vm, Container, Host |
| `/run/feos/api.sock` | Container, Host, Image |
| `/run/feos/image.sock` | Image, internal |
| `/run/feos/task.sock` | Task, internal, in the task service process |
| `<vm dir>/v.sock_10001`, per VM | Lifecycle, for the guest's uplink |

In a pod (`feos` built with `guest`, to become `feos-guest`):

| Listener | Services |
|---|---|
| vsock port 10000, `/run/feos/api.sock` | Container, Host, Image, Tool, Watch, PortForward, Kube, SSH |
| `/run/feos/image.sock`, `/run/feos/task.sock` | Image and Task, internal |

The guest dials the host on vsock port 10001 (the uplink) and pipes
Kubernetes and SSH connections to vsock ports 10002 and 10003.

Decided, not built ([ADR 0006](../adr/0006-control-transport.md)): every
public listener serves the same services; `feosd` listens on
`/run/feos/api.sock` always, on Valkey when configured, and on TCP only
when configured.

## Compatibility

- **The guest's API only grows.** A suspended pod keeps the `feos-guest`
  it was suspended with, so a newer `feosd` talks to older guests. Within
  v1, fields and RPCs are added, never renumbered, removed or given another
  meaning. This holds for `feos.lifecycle.v1` and every package
  `feos-guest` serves.
- **The node's API** is what the control plane calls. It follows the same
  rule within v1; a break is a new version of the package.
- **Internal APIs** (`feos.task.v1`) change freely: both ends are one
  binary.
