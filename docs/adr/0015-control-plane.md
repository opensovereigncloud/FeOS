# ADR 0015: FeRun, one replicated control plane that nodes connect to over mTLS

**Status:** Proposed · **Date:** 2026-10-15

## Context

ADR 0001 leaves the control plane as "one or two central services" that
reach nodes only through Valkey, and ADR 0006 proposed the transport:
`feosd` reads its node's command stream, a bridge hands each command to
the gRPC router and writes the responses back. Nothing of it is built.
The sandbox system's control plane, `sandbox-api`, still runs in its own
repository: about 13,000 lines of Go and a management SPA, a SQLite
registry, one instance, and Valkey between it and its managers. FeCode,
the coding harness, is its first client: FeCode's backend creates
sandboxes over the REST API, holds a sandbox token per workspace, forwards
previews through the port front door, and states each sandbox's
Kubernetes and SSH bindings. Sandboxes and CaaS are one part of FeCode's
runtime, beside its git and its models.

Three things have to be decided together: what the successor of
`sandbox-api` is, how it is replicated, and how it and the nodes talk.
What the sandbox system learned from its transport shapes the third:

- Its ADR 0007 chose Valkey streams for three reasons: the envelope would
  be the multi-node protocol, tool calls would queue behind long lifecycle
  operations, and redelivery would survive manager restarts. In practice
  the manager advanced its read offset after dispatch, so a restart lost
  queued work, and callers reconciled and retried instead. The queue's
  ordering made everything behind one long operation wait, until cancels
  and inventory requests were taken out of the sequence. A command for a
  node that had gone away sat in the queue until the API learned to fail
  such calls fast.
- Valkey carries no byte streams. For port forwarding the manager opened
  an HTTP listener on every node, authenticated with one shared bearer,
  and the API dialed it. The Kubernetes and SSH bindings then took the
  same path, because every component with the Valkey credential could read
  the bus. The node listened after all, and the platform had one
  credential for all nodes.
- The control plane's model was borrowed from Nomad: registration with a
  fingerprint, telemetry as the heartbeat, an inventory resync, one
  reconcile loop with a periodic sweep, every edge-triggered path backed
  by a level-triggered one. Nomad's own servers reach their clients over
  the connections the clients opened. Only the transport stayed a broker.

FeOS has the same needs. ADR 0004 lists the console, port forwards and
interactive exec as streams the Valkey transport cannot carry; ADR 0014
rests the pods' identities on Valkey's ACLs and keeps secrets off the bus
by sealing them. Deployments range from one machine, control plane and
node together, to 512 nodes behind a control plane that survives the loss
of an instance.

## Decision

### One program, `ferund`, in replicas

- The control plane is one program, `ferund`, the successor of
  `sandbox-api`. It holds everything that is more than one node's business
  (ADR 0001): tenants and their tokens, the registry of sandboxes and
  services, placement, the policies that pause, hibernate and scale to
  zero, the front doors, the fleet of nodes, and the issuer of ADR 0014.
- It is FeRun, the part of FeCode's runtime that runs sandboxes and
  services, and stays a service of its own with its own surfaces:
  FeCode's app is one client of it. FeOS is what runs on nodes
  (ADR 0001). The daemon is `ferund`, named like `feosd`; `feosctl`
  stays the node's client.
- It stays in Go and moves into this repository, into the top-level
  directory ADR 0002 reserves for Go. Its REST, MCP and port surfaces keep
  the sandbox system's decisions; those come into this directory as their
  parts are built here.
- It runs as identical instances: three in production, one on a single
  machine. An instance holds no state of its own, only caches, its
  connections to the nodes and the leases it holds. Any instance serves
  any request.

### One database, one leader

- The state is in PostgreSQL: the registry, tenants, tokens, nodes,
  bookings, events and audit. PostgreSQL runs with replicas and automatic
  failover; `ferund` sees a failover as a reconnect. The database is
  the only part that is made highly available by something other than
  running the program again.
- What the sandbox system serialised with mutexes in one process is a
  transaction: placement books a pod's size on a node under a lock on the
  node's row, and a sandbox's or a service's transition under a lock on
  its row. Two instances cannot book the same room or start the same pod
  twice.
- One instance at a time is the leader, by a lease in the database that
  it renews and that expires when it stops. The leader runs what must run
  once: the expiry of nodes that stopped reporting, the grace before their
  pods count as lost, the reconcile sweep, the idle, hibernation and
  scale-to-zero policies, and the re-issue of drains. What a request or an
  event triggers runs on the instance that received it, under the row's
  lock.
- Every instance holds the issuing key of ADR 0014, an intermediate of the
  platform CA, and the database credential. Those are the control plane's
  two secrets.

### Nodes connect out, to every instance, over mTLS

- `feosd` opens connections to every instance of the control plane: it
  resolves one name to all their addresses, or takes a list, and
  reconnects with backoff. The control plane opens nothing to a node.
  Requirement 7 of ADR 0001 stands: the node has no port for control.
- Every connection is TLS 1.3 with a certificate on each side, from the
  platform CA of ADR 0014. The instance's certificate names the control
  plane; the node's carries the node's SPIFFE ID. The control plane knows
  which node a connection belongs to from the certificate, never from what
  the node sends.
- Between a node and an instance there are two connections, because
  HTTP/2 fixes which end of a connection is the client:
  - **The node connection.** The node dialed, and on it the node is the
    HTTP/2 server. The instance calls the `feos.*` APIs over it as
    `feosctl` does over the Unix socket: `feos.pod.v1`, `feos.network.v1`
    and the rest, unary calls and streams of every kind, calls for a pod
    with `feos-pod-id` (ADR 0004). This is `feosd`'s third listener, next
    to the Unix socket and TCP for labs, and it serves the same services.
  - **The uplink.** The node is the client. `feos.fleet.v1` is the control
    plane's API for nodes: joining and renewing the node's certificate,
    telemetry, events with acknowledgements, and the credential requests
    of ADR 0014, a pod's signing request and HPKE key in, its certificate,
    trust bundle and sealed bindings out. The node uses the uplink to one
    instance at a time and moves to another when it fails.
- There is no queue and no cache of answers. A call returns its gRPC
  status, or fails with `UNAVAILABLE` when the connection is lost, and
  the caller retries: the APIs are idempotent by construction (the caller
  chooses a pod's ID, a group's update names `from_version`). An instance
  whose connection to a node is down waits briefly for the node to come
  back, then fails the call. The pod stays where it is; the reconcile loop
  decides what becomes of it.
- The nodes' port of the control plane carries TLS end to end. What stands
  in front of the instances for it passes TCP through: a layer-4 balancer,
  or the instances' own addresses in DNS. A proxy that terminates TLS or
  reads HTTP/2 would break the node connection.

### The node's identity

- A node's certificate carries `spiffe://<trust domain>/node/<name>`. It
  is short-lived and renewed over the uplink at half its lifetime, so
  that a node switched off for up to half of it needs nothing; a pod's
  at three quarters (ADR 0014). The private key lives in the node's
  state directory, readable by root alone; a TPM is a later ADR.
- The first certificate comes from a join. `feosd` presents a one-time
  join token, issued by an operator through the control plane, over a
  connection that authenticates the control plane only, with a signing
  request for the name it proposes, its hostname. The control plane
  assigns the name, or refuses one that a live node holds, and issues.
  From then on everything is mTLS.
- That a signing request for pod X comes from node N rests on the node's
  certificate, which closes ADR 0014's dependence on Valkey's ACLs.

### Telemetry, events and liveness

- A node reports telemetry on its uplink every few seconds: memory, CPU,
  its pods, the versions it has (ADR 0009), and a hash of its inventory.
  The report is the heartbeat. The receiving instance writes `last_seen`
  to the node's row; the leader marks a node down when the row ages past
  a TTL, and its pods lost after a grace, never on one missed report. A
  node's connections dropping tells an instance to fail calls for it
  fast; the fleet's truth is the row.
- A node's events, the pods' lifecycle, `feos-authn`'s audit records
  (ADR 0014) and the traffic records of ADR 0013, go over the uplink with
  a dense sequence number per start of `feosd`. The node keeps them on
  disk, bounded, until the control plane has acknowledged them, and
  continues after the last acknowledged number when it reconnects, to
  whichever instance. The database dedupes on node, start and sequence; a
  gap is recorded as a gap. What overflows the node's buffer is dropped
  and counted (ADR 0013).
- The control plane is the only reconciler. Nodes report what is and
  execute what they are called with; they hold no desired state. On every
  reconnect, and on an inventory hash that disagrees with the registry,
  the control plane lists the node's pods and reconciles.

### Front doors

- The REST API, MCP, the management UI and HTTP to the ports of pods are
  served by every instance behind an ordinary HTTPS balancer, without
  sticky sessions. The bearer is the sandbox; an MCP session ID is signed
  and self-describing, so any instance accepts it. An exec, a tool call or
  a port forward goes over the serving instance's own node connection. A
  broken stream is the client's to reopen, as it was with the sandbox
  system's forward listener.
- Inbound traffic to services, CaaS's ingress, is not a front door of
  `ferund` (ADR 0001, open questions).

### From one machine to 512 nodes

- One shape of deployment. On one machine, `ferund` and PostgreSQL
  run beside `feosd`, which dials the control plane on the loopback with
  the same mTLS. In production, three instances behind a TCP balancer for
  the nodes and an HTTPS balancer for the front doors, over a PostgreSQL
  with replicas. Nothing else runs.
- With 512 nodes and three instances the control plane holds about 3,000
  TLS connections, and HTTP/2 multiplexes every call to a node over its
  one connection. Telemetry is about a hundred row updates a second. Both
  are far below what one instance handles; the limits are elsewhere (open
  questions).
- `ferund` ships as a container image and, for the single machine,
  as a `.deb` (ADR 0009).

## Alternatives considered

- **Valkey streams** (ADR 0006, the sandbox system). Valkey checks a
  client's certificate but maps it to no ACL user, so a node's identity
  stays a password per node. No byte streams, so the console, port
  forwards and exec need a second path, which on the sandbox system was a
  listener on every node with a shared bearer, and the secrets followed
  it. The redelivery it promised was given up in practice, and the queue's
  ordering was a liability. A third system that holds one credential for
  every node, for a transport that one TLS listener gives.
- **NATS.** The better broker: certificates map to users, permissions per
  subject confine a node to its own, deduplication by message ID,
  retention, clustering, request and reply. Still no byte streams, so the
  second path stays; and still a third system to run, secure and upgrade
  for what one listener on the control plane does. If the control plane
  ever needs a bus between its instances, NATS is the candidate; the node
  would not see it.
- **Nodes listening, the control plane dialing in.** Every node a TLS
  server that authenticates its callers, reachable from wherever the
  control plane runs; against requirement 7 of ADR 0001.
- **One connection per node, both directions on it.** A multiplexer under
  HTTP/2, or a message schema on a bidirectional stream, which is the
  second API ADR 0006 rejected. A second TLS connection costs nothing.
- **Nodes connect to one instance; instances forward calls to each
  other.** A routing layer the node connection does not need while the
  instances are few. It can be added behind the same node API when they
  are many.
- **Front doors as a service of their own** (ADR 0001, open questions).
  Every exec, tool call and port forward would hop through the core
  service to reach a node connection.
- **Consensus in `ferund` instead of PostgreSQL.** No database to
  run, but a consensus implementation and its storage to own, and a query
  layer to write. The sandbox system judged Nomad's pipeline too slow for
  its dispatch; a store of our own would not come faster.
- **SQLite on one machine, PostgreSQL in production.** Two dialects, two
  test matrices, and the one-machine deployment would not exercise the
  path production runs.
- **A port to Rust**, one language, the proto crates shared. Nothing in
  the control plane needs what Rust gives the node, and 13,000 working
  lines and the SPA would be rewritten before anything new is built.

## Consequences

- Valkey leaves the platform. ADR 0006 is superseded. ADR 0001's sentence
  that the control plane reaches nodes only through Valkey becomes "only
  over the connections the nodes open to it". ADR 0012 sends groups over
  the node connection, ADR 0013 records over the uplink. ADR 0014's sealed
  bindings travel inside TLS, and the sealing stays, for `feosd`'s memory,
  logs and dumps.
- ADR 0004's open question on streams from outside the node is answered:
  the console, port forwards and, when the API has it, interactive exec
  are streams on the node connection.
- `feosd` gains a dialer with backoff to every instance, the node
  connection as a listener that feeds the router it has, the uplink
  client, a key and certificate with their renewal, and the event buffer
  with its sequence and acknowledgements. The bridge, the request cache
  and the Valkey client of ADR 0006 are not built.
- A node that reaches no instance keeps running its pods, buffers its
  events, and serves `feosctl` on its socket. It takes no new work,
  because nothing can hand it any.
- The control plane's registry moves from SQLite to PostgreSQL, its
  mutexes become transactions, and a leader lease appears. The sandbox
  system's transport package goes; its `Transport` interface becomes gRPC
  clients over the node connections.
- The control plane has two listeners with different fronts: TCP
  passthrough for the nodes, HTTPS for the front doors.
- An instance that loses a node's connection fails calls for that node
  until the node is back, seconds under normal conditions. Clients of the
  front doors see `UNAVAILABLE` and retry.

## Open questions

- Where `feosd` gets the control plane's name, the join token and its
  state directory: a file in `/etc/feos`, systemd credentials, or the
  kernel command line under `feos-host-init` (ADR 0006's question,
  unchanged).
- Attestation at join: the node's TPM (ADR 0014).
- Traffic records at 512 nodes: how much a node aggregates or samples
  before it sends, and whether PostgreSQL is where they rest or only where
  they pass through.
- When the instances are many: the routing layer among the alternatives.
- The control plane's own bootstrap: who holds the platform CA's root, and
  how the intermediate reaches the instances.
- The trust domain and the layout of the IDs (ADR 0014).
