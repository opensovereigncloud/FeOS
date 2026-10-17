The API
=======

The `feos.*` gRPC APIs are the contract between the node and the control
plane, and between `feosd` and `feos-guest`. They live in
`proto/feos/<api>/v1/`, in the package `feos.<api>.v1`.

## Packages

| Package | What it is for | Served by today |
|---|---|---|
| `feos.pod.v1` | pods: create, start, stop, delete, suspend, resume, hibernate, restore, console, events | `feosd` |
| `feos.volume.v1` | persistent volumes: create, delete, get, list; a pod's spec names them ([node.md](node.md#volumes)) | `feosd` |
| `feos.disk.v1` | the pods' disks and memory over a chunk store and the object store, internal to the node ([disk.md](disk.md)) | `diskd` |
| `feos.network.v1` | the pods' security groups (`SecurityGroupService`) and the records of their traffic (`TrafficService`) | `feosd` |
| `feos.fleet.v1` | the control plane's API for nodes, on the uplink: join, the node's certificate, telemetry, events, pods' credentials ([control-plane.md](control-plane.md#nodes)) | `ferund` |
| `feos.microvm.v1` | the pods' microVMs, internal to `feosd`, served on no listener | – |
| `feos.container.v1` | containers: create, start, stop, delete, exec, logs, events | `feosd`, `feos-guest` |
| `feos.image.v1` | OCI image pulls and the image store | `feosd`, `feos-guest` |
| `feos.host.v1` | the machine: hostname, memory, CPU, network, version, logs, power, upgrade; and, for now, the guest's pushed time and network, freezing and `PreparePause` | `feosd`, `feos-guest` |
| `feos.task.v1` | the container runtime, internal between the main process and the task service | both, internally |
| `feos.lifecycle.v1` | the guest's uplink: `Attach` is held open while the guest runs | `feosd` (server), `feos-guest` (client) |
| `feos.guest.v1` | what only a guest does for its host: `MountVolumes` ([guest.md](guest.md#volumes)) | `feos-guest` |
| `feos.tool.v1` | an agent's file and web tools on a container's files: read, write, edit, ls, rg, fd, webfetch | `feos-guest` |
| `feos.watch.v1` | inotify subscriptions on a container's files | `feos-guest` |
| `feos.portforward.v1` | streams to ports inside the guest | `feos-guest` |
| `feos.kube.v1`, `feos.ssh.v1` | the listeners for Kubernetes and SSH credential offloading | `feos-guest` |

Decided, not built:

- `feos.guest.v1.GuestService`, which has `MountVolumes`, takes
  `SetTime`, `GetTime`, `SetNetworkConfig`, `GetNetworkConfig`,
  `FreezeFilesystem`, `ThawFilesystem` and `PreparePause` out of
  `HostService`; only `feos-guest` serves it
  ([ADR 0007](../adr/0007-guest.md)).

## Listeners

On a node (`feosd`):

| Listener | Services |
|---|---|
| `/run/feos/api.sock`, and TCP with `--tcp` | Pod, Volume, SecurityGroup, Traffic, Container, Host, Image; and a pod's Container, Image, Tool and Watch |
| `/run/feos/image.sock` | Image, internal |
| `/run/feos/task.sock` | Task, internal, in the task service process |
| `<vm dir>/v.sock_10001`, per VM | Lifecycle, for the guest's uplink |

In a pod (`feos-guest`):

| Listener | Services |
|---|---|
| vsock port 10000, `/run/feos/api.sock` | Container, Host, Image, Tool, Watch, PortForward, Kube, SSH, Guest |
| `/run/feos/image.sock`, `/run/feos/task.sock` | Image and Task, internal |

The guest dials the host on vsock port 10001 (the uplink) and pipes
Kubernetes and SSH connections to vsock ports 10002 and 10003.

A call to `feosd` that carries the metadata `feos-pod-id` goes unchanged
to that pod's guest, on vsock port 10000, if it is for the guest's
Container, Image, Tool or Watch service and the pod is pending or running
([ADR 0004](../adr/0004-pods.md)); for any other service it fails with
`UNIMPLEMENTED`. `feosctl` sends it with `--pod <id>`.

Every public listener serves the same services. TCP has no
authentication; `feos-host-init` starts `feosd` with `--tcp [::]:1337`.
With `--control-plane`, `feosd` serves them on the node connections it
opens to the control plane too ([ADR 0015](../adr/0015-control-plane.md),
[node.md](node.md#the-control-plane)).

## The pods' network

A pod's network ([ADR 0010](../adr/0010-pod-network.md),
[node.md](node.md#the-pods-network)) is in its spec and its record:

- `PodSpec.network` (`PodNetwork`): `none` for no network at all;
  `security_groups`, the IDs of the pod's security groups, the node's
  default group when empty; `dns_upstreams`, the IP addresses of the
  resolvers that `feosd`'s resolver asks for the pod, the node's when
  empty. A create with an invalid group ID or an upstream the pod may not
  use fails with `INVALID_ARGUMENT`; one with a network on a node without
  `--pod-prefix`, or whose network is not set up, fails with
  `FAILED_PRECONDITION`.
- `PodSpec.metadata`: keys and values that the pod reads from its
  metadata service at `169.254.169.254`, beside `feosd`'s own, the pod's
  ID and addresses ([node.md](node.md#metadata)); no credentials, which
  anything in the pod could read. A create with a key other than 1 to
  128 letters, digits, `-`, `_` and `.`, with more than 32 KiB as JSON,
  or with metadata and no network fails with `INVALID_ARGUMENT`.
  `feosctl pod create --metadata KEY=value`.
- `PodInfo.ipv6_address`: the pod's address on the network, of the
  node's prefix, while it is on the node. `PodInfo.ipv4_address`: its
  IPv4 address on the node, of `--pod-ipv4-range`, for leaving through
  masquerade; another pod may have it once the pod is deleted.

`feos.network.v1` is the node's side of ADR 0012 and ADR 0013:

- `SecurityGroupService`: `PutSecurityGroup` replaces a group whole;
  `UpdateSecurityGroup` adds and removes members from `from_version` to
  `version`, and fails with `FAILED_PRECONDITION` unless the node has the
  group at `from_version`; `DeleteSecurityGroup` fails with
  `FAILED_PRECONDITION` while a pod names the group; `GetSecurityGroup`
  and `ListSecurityGroups`. A rule is a direction, a protocol (TCP, UDP,
  ICMP, ICMPv6 or any), ports of TCP and UDP, and a peer: an address block
  with exceptions, of either family, or a group's members, of IPv6. An
  IPv4 block counts in rules out alone: nothing new comes in over IPv4. A
  change applies to the pods' rules within moments of the call.
- `TrafficService.StreamTrafficRecords` streams flows, drops, DNS queries
  and server names as `feosd` makes them, of one pod or of all; what a
  slow call misses is counted in the next record's `missed`.

The guest's network configuration (`SetNetworkConfigRequest`,
`GetNetworkConfigResponse`, `feos.microvm.v1.GuestNetworkConfig`) gained
`ipv6_cidr` and `ipv6_gateway` by addition; `feosd` pushes both
families: the IPv6 /128 via `fe80::1`, the IPv4 /32 via `169.254.0.1`,
and its resolver ([guest.md](guest.md)).

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
