# ADR 0006: The control plane's calls travel through Valkey

**Status:** Proposed · **Date:** 2026-10-14

## Context

ADR 0001 requires that nodes open no port for control. The sandbox system
met that with Valkey streams between its control plane and its managers:

- a command stream per node (`sb:cmd:<node>`), read by the node's manager
  in a consumer group;
- a response stream per API instance (`sb:resp:<api>`), events per node
  (`sb:events:<node>`), and the fleet's telemetry for placement
  (`sb:nodes`);
- at-least-once delivery: the manager acknowledges a command after its
  terminal frame and answers a redelivered one from a cache keyed by
  `request_id`, so commands survive restarts and queue behind long
  operations.

Its commands had a schema of their own, next to the gRPC APIs the manager
spoke to the guest. FeOS's host profile, in turn, served its API on TCP
`[::]:1337`, without authentication.

## Decision

### feosd connects out to Valkey

`feosd` connects to Valkey itself, over TLS, and reads its node's command
stream in a consumer group. The node opens no port.

### Valkey carries gRPC calls

Valkey is a transport for `feosd`'s gRPC calls, not a second API with
operations of its own.

- A command is `{request_id, reply_to, method, metadata, deadline,
  payload}`: the path of the gRPC method
  (`/feos.pod.v1.PodService/CreatePod`), its metadata (`feos-pod-id` among
  it, ADR 0004) and the request message.
- A bridge in `feosd` hands the call to the router that serves
  `/run/feos/api.sock` and writes each response message as a frame to
  `reply_to`, then a terminal frame with the gRPC status.
- The bridge keeps the delivery guarantees, once for all services: it
  acknowledges a command after its terminal frame, answers a redelivered
  `request_id` from a cache of its frames, and drops a call whose deadline
  has passed.
- Unary and server-streaming calls go over Valkey. Client and
  bidirectional streams do not.
- `feosd` publishes its pods' events on `sb:events:<node>` and its
  telemetry on `sb:nodes`: memory, CPU, pods, and the Firecracker and guest
  versions it has (ADR 0009).

The control plane calls the `feos.*` APIs through a gRPC client that uses
this transport. The sandbox system's command schema goes.

### Listeners

`feosd` listens on `/run/feos/api.sock` always, on Valkey when it is
configured, and on TCP only when it is configured. The TCP listener has no
authentication and is for labs. Every listener serves the same services.

## Alternatives considered

- **Valkey with a command schema of its own**, as the sandbox system has.
  A second API next to the gRPC one, to keep in step with it, and the
  delivery guarantees in every operation instead of once.
- **The control plane calling nodes over gRPC on TCP**, as FeOS's API was
  meant to be used. Every node would open a port and authenticate its
  callers, and commands would no longer be buffered and redelivered across
  restarts of `feosd`.
- **A separate process on the node that consumes Valkey** and calls
  `feosd` on its socket. One more program to deploy and version, for a
  translation the bridge does generically.

## Consequences

- The control plane moves to the `feos.*` APIs.
- `feosctl` uses the Unix socket on the node; from elsewhere it goes
  through SSH.

## Open questions

- **Configuration.** Where `feosd` gets the Valkey endpoint, its
  credentials and the node's name: a file in `/etc/feos`, systemd
  credentials, or the kernel command line under `feos-host-init`.
- **Valkey ACLs** that limit a node to its own streams.
- **Streams from outside the node** (ADR 0004).
