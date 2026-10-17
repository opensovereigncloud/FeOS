The Control Plane
=================

What FeRun's control plane is and how it works: `ferund`, in `go/`, and
its management UI, in `web/`. The decisions behind it:
[ADR 0015](../adr/0015-control-plane.md) (the control plane, its
replication, and how nodes connect),
[ADR 0014](../adr/0014-identity-and-credentials.md) (identity and
credentials), [ADR 0013](../adr/0013-traffic-records.md) (traffic records),
[ADR 0017](../adr/0017-pod-mesh.md) (the pods' mesh),
[ADR 0009](../adr/0009-node-os-and-packages.md) (packages). The node's side
of the wire is in [node.md](node.md#the-control-plane).

ferund is the successor of the sandbox system's control plane. Its REST
API, MCP and port front door keep that system's paths, bodies, status codes
and texts, which FeCode's backend and the UI were built against; what is
new is underneath: PostgreSQL instead of SQLite, replicas instead of one
process, and nodes that connect over mTLS instead of a broker.

Parts that are decided but not built are marked as such.

## Instances

`ferund` runs as identical instances over one PostgreSQL database: three
in production, one on a single machine. An instance holds no state of its
own beyond caches, its connections to nodes and the leases it holds; any
instance serves any request. Each has two listeners:

| Listener | Default | Serves | In front of it |
|---|---|---|---|
| the front doors, `--listen` | `:8100` | the REST API, MCP at `/mcp`, the ports at `/fwd/`, sign-in at `/auth/`, the management UI, `/healthz`, `/readyz` | an HTTPS balancer, without sticky sessions; or TLS of its own with `--tls-cert` and `--tls-key` |
| the nodes' port, `--node-listen` | `:7443` | node connections, uplinks and joins, TLS 1.3 always | a TCP passthrough, or the instances' own addresses |

`ferund` has four subcommands:

    ferund [serve] [flags]          run an instance (the default)
    ferund init-ca --root-dir R --dir D --dns-domain X [--dns-domain Y ...]
                   [--trust-domain T] [--control-plane-domain C]
    ferund join-token --database-url URL [--ca-dir D] [--trust-domain T]
                      [--ttl 1h] [--node NAME]
    ferund migrate --database-url URL

Every flag has an environment variable `FERUN_<NAME>`, the flag's name in
upper snake case (`--database-url`: `FERUN_DATABASE_URL`); a flag on the
command line wins over its variable, and an empty variable counts as
unset. Durations are written in Go's syntax (`90s`, `10m`, `12h`), byte
sizes as integers with an optional `KiB`, `MiB` or `GiB` suffix.
`ferund <command> --help` lists the flags with their variables and
defaults. `serve` migrates the database at its start; `migrate` does only
that.

### serve's flags

| Flag | Default | What it is |
|---|---|---|
| `--database-url` | required | the PostgreSQL DSN |
| `--listen` | `:8100` | the front doors |
| `--tls-cert`, `--tls-key` | none | TLS on `--listen`; without, plain HTTP behind the balancer. Loaded at the start, which fails on a missing or bad file, and read again every minute: a pair renewed on disk is served from the next handshake on, and one that does not load keeps the pair before |
| `--node-listen` | `:7443` | the nodes' port |
| `--node-advertise-address` | none | `host:port` at which nodes reach this instance's nodes' port; listed to nodes. Empty: the instance is not listed, and nodes reach it only by the name they were configured with (one machine) |
| `--instance-id` | `<hostname>-<8 random hex>` | the instance's ID; the default is new at every start. A fixed one (a StatefulSet's pod name) must be unique among the running instances: a start under an ID whose row was written within 15 s waits until the row is older, and is refused if it is written meanwhile ([Leader and instances](#leader-and-instances)) |
| `--trust-domain` | `feos.local` | the SPIFFE trust domain |
| `--ca-dir` | `/etc/ferun/ca` | the issuer: `issuer.crt`, `issuer.key`, `issuer.json`, `bundle.crt`; never `root.key` |
| `--node-server-names` | the control-plane domain | the DNS names of the instance's certificate on the nodes' port, each under the control-plane domain |
| `--secret-key-file` | none | 32 bytes (raw, hex or base64) that seal the bindings at rest |
| `--bindings` | off | serve the Kubernetes and SSH bindings; only with `--secret-key-file` |
| `--deny-prefix` | a built-in set | a destination that preflights, bindings and the image resolver may not reach, repeatable |
| `--admin-users` | none | `user:password,...` for the admin API: Basic, beside or instead of OIDC |
| `--public-url` | none | the front door's external URL, e.g. `https://run.fecode.dev`; required with `--oidc-issuer` |
| `--oidc-issuer` | none | the OIDC issuer of human sign-in, exactly as its discovery document states it |
| `--oidc-client-id` | `ferun` | |
| `--oidc-client-secret-file` | none | the client's secret; required with `--oidc-issuer` |
| `--oidc-scopes` | `openid profile email` | space-separated, with `openid` |
| `--oidc-admin-group` | `admins` | a person is an admin when the `groups` claim holds it |
| `--session-ttl` | `12h` | the absolute lifetime of a UI session |
| `--bootstrap-token` | none | becomes a token of the tenant `operator` while no tenant exists |
| `--mcp-allowed-origins` | none | the Origins `/mcp` accepts; empty refuses every request with an Origin |
| `--pod-network` | empty | the pods' network ([The pods' mesh](#the-pods-mesh)): empty, routed, every node with a prefix of its network's; `mesh`, a ULA /48 drawn at the first start and kept in the database; or such a /48, `fdXX:XXXX:XXXX::/48` |
| `--default-image` | `ghcr.io/maltej/debian-ai:latest` | the image of a sandbox created without one |
| `--default-vcpus`, `--default-memory-mib` | 2, 512 | the size of a sandbox created without one |
| `--placement` | `binpack` | or `spread` |
| `--disk-budget` | `4GiB` | placement's disk gate per pod |
| `--idle-pause` | `10s` | a running sandbox idle this long is paused; 0 disables |
| `--hibernate-after` | 0 | a sandbox paused this long is hibernated; 0 disables |
| `--node-ttl` | `15s` | the age of a node's telemetry before it is down |
| `--lost-grace` | `10m` | how long a node is down before its sandboxes count as lost |
| `--node-wait` | `5s` | how long a call waits for a node's connection |
| `--rollout-grace` | `30s` | how long a call waits for a node that announced its restart |
| `--drain-timeout` | `2m` | how long a draining node waits for the execs still running in its sandboxes; then they are cancelled and the sandboxes move |
| `--pending-repair-after` | `10m` | a pod pending this long without an op is repaired |
| `--unreachable-repair-after` | `2m` | a running pod whose guest stays unreachable this long is repaired |
| `--node-cert-ttl`, `--pod-cert-ttl` | `168h`, `1h` | the certificates' lifetimes; a node's is renewed at half of it, so that a node switched off for up to half of it needs nothing, a pod's at three quarters |
| `--node-max-streams` | 1024 | the HTTP/2 stream limit nodes advertise on a node connection; the stream budgets derive from it |
| `--conn-drain-timeout` | `20m` | a draining node connection is closed after this at the latest |
| `--hibernate-timeout` | `1h` | how long ferund waits for a hibernation, plus 16 s per GiB of memory; on a feosd that reports its hibernations, the node's goes on after it |
| `--shutdown-grace` | `10m` | how long a stopping instance waits for its requests and ops |
| `--activity-retention`, `--traffic-retention` | `720h`, `168h` | how long activity and traffic records are kept |
| `--log-level` | `info` | |

Fixed timings, not flags (`internal/config`): an instance's heartbeat every
5 s, live for 15 s; the leader lease 15 s, renewed every 5 s; a sandbox's
preparation before a tool call at most 7 min 5 s (REST advertises
`preparationWaitMs: 425000`); telemetry every 5 s; one node's inventory
reconcile at most every 30 s; holds leased for 30 s and renewed every
10 s; join tokens valid 1 h when no lifetime is given; the instance's
certificate on the nodes' port 24 h.

## Packages

`go/` is the module `github.com/ironcore-dev/feos/go`. The generated code
of `proto/` is in `go/gen/feos/<api>/v1` (`make go-gen`), the rest in
`go/internal/`:

| Package | What it is |
|---|---|
| `config` | the flags and their variables, the fixed timings |
| `db` | the connection pool, transactions with retries, the embedded schema and its migration, notifications |
| `store` | typed queries over the schema, one file per domain |
| `model` | value types and errors that several packages share |
| `activity` | ferund's rows of the activity log, written in batches; the partitions of the log |
| `pki` | the CA: issuer, certificates, signing requests, SPIFFE IDs, join tokens and pins, HPKE |
| `mesh` | the pods' mesh: the ULA, a node's /64, endpoints, the peers' deltas |
| `nodelink` | the nodes' port: TLS, ALPN, the node connections and their budgets, the uplink, join and refusal servers |
| `fleet` | `feos.fleet.v1`: joins, renewals, telemetry, events, pods' credentials, the mesh, the nodes' liveness; the mesh's peers to the nodes |
| `leader` | the leader lease and the leader's loops, the instance's heartbeat |
| `sandbox` | the sandbox engine: states, ops, placement, sync, reconcile, tombstones, policies, images |
| `tools` | the agent's tools in a sandbox, and their REST routes |
| `mcpsrv` | the MCP door |
| `ports` | the port door |
| `watch` | file watches across instances |
| `bindings` | Kubernetes and SSH bindings |
| `api`, `auth`, `apierr` | the REST router, its auth classes and errors, human sign-in |
| `ui` | the embedded build of `web/` |
| `server` | one instance, built from its configuration, and its stop |
| `testnode` | a Go implementation of feosd's side of the wire, for tests and development |
| `e2e` | end-to-end tests: several instances and test nodes on one database |

`cmd/ferund` is the daemon; `cmd/ferun-testnode` runs a test node
([below](#the-test-node)).

## The database

The state is in PostgreSQL, 17 or newer: the schema declares identity
columns on partitioned tables, and `migrate` refuses an older server with
"ferund needs PostgreSQL 17 or newer". A database with replicas must
replicate synchronously ([Operations](#operations)).

- **Sessions.** Every connection of the pool runs with `lock_timeout`
  5 s, `statement_timeout` 30 s and the time zone UTC.
- **Transactions** run at READ COMMITTED with explicit row locks.
  Each statement sees its own snapshot, so a check that must see what
  another transaction committed while this one waited for a lock is a
  separate, later statement. A transaction that fails with a
  serialization failure, a deadlock or a lock timeout runs again, up to
  five times with jittered backoff. Nothing slow, no call to a node, runs
  inside a transaction.
- **Time.** Every lease, TTL, grace and deadline is decided on the
  database's clock (`now()`), never on a node's or an instance's.
- **Migrations** are embedded (`internal/db/migrations/`: `0001_init.sql`,
  the schema; `0002_auth.sql`, sign-in;
  `0003_policy_backoff_and_placements.sql`, the policies' backoff
  ([Policies](#policies)) and an empty `placements`;
  `0004_traffic_node_foreign.sql`, the partial index of a node's foreign
  traffic records ([Nodes](#nodes)); `0005_mesh.sql`, the pods' mesh) and
  applied under an advisory lock, recorded in `schema_migrations`. The
  same locked section creates the cluster's MCP session key, reconciles
  `--pod-network` with the database's ([The pods'
  mesh](#the-pods-mesh)) and, with `--bootstrap-token`, creates the tenant
  `operator` and its token, when no tenant exists.
- **Notifications.** One connection per instance listens on the channels
  below and fans them out in the process. A writer sends its notification
  in the transaction that made the change, so it arrives at commit. After
  the listener reconnected, every subscriber gets an empty payload: "you
  may have missed something, read again". Waiting for another instance's
  write (`db.WaitFor`) always subscribes first, then reads, then waits for
  a notification or 500 ms, and reads again.

| Channel | Payload | Sent when |
|---|---|---|
| `ferun_sandbox` | a sandbox ID, or `*` for more than 256 | a sandbox's state, op, node, incarnation or checkpoint changed, or it was created or deleted |
| `ferun_node` | a node name | its certificates were revoked or superseded, it was removed, its identity forked, its restart was announced, its draining changed |
| `ferun_reconcile` | a node name | an instance not connected to the node asks one that is to reconcile it |
| `ferun_leader` | a hint | the leader's loops have work |
| `ferun_watch` | a watch ID | a watch's paths, owner or result changed |
| `ferun_cancel` | JSON | an exec or an MCP call is to be cancelled on whichever instance runs it |

The tables: `instances`, `leases`, `cluster_secrets`; `tenants`,
`tenant_tokens`; `nodes`, `node_telemetry` (the hot row, about a hundred
updates a second at 512 nodes, never locked by placement), `node_certs`
(every node certificate issued), `join_tokens`, `node_starts`,
`event_gaps`; `pod_network` (the mesh's ULA and its peers' version, one
row while the cluster is meshed); `sandboxes`, `sandbox_tokens`, `sandbox_holds`,
`pod_tombstones`, `bookings`; `activity` and `traffic_records`,
partitioned by day; `issued_certs` (pods' certificates); `watches`;
`kube_bindings`, `ssh_bindings`; `auth_logins`, `ui_sessions`. Tokens are
stored as SHA-256 hashes only, and shown once, at their creation.
`placements`, which held the placement decisions, is written and read by
nothing since `0003` emptied it, and goes in a later migration: the
previous version, which a rolling deploy runs beside this one, still
inserts into it in every create's and restore's begin. A placement's
audit is its activity row.

## Leader and instances

- **Instances.** Every instance writes its row in `instances` every 5 s.
  An instance is live while its row is younger than 15 s and not marked
  stopping. The leader deletes rows not seen for an hour.
- **The start** claims the instance's ID before anything serves, in one
  transaction: its row is written anew (started now, not stopping), and
  the ops the ID still owns are released as in a stop's step 4, since no
  process runs them: they are an earlier run's that ended without its
  stop, crashed or killed. Their sandboxes say "op <op> of instance <id>
  was released: the instance started again". A row younger than 15 s,
  stopping or not, is another process's under the same ID, or a run's
  that ended less than 15 s ago: the start tries again every second until
  the row is older, and is refused when the row is written meanwhile.
- **The lease.** One instance at a time leads, by the row `leader` of
  `leases`: a TTL of 15 s and an epoch that grows by one on every
  acquisition, also by the holder of an expired lease. The epoch is the
  fencing token. The leader renews every 5 s and steps down at once when a
  renew fails or finds the lease gone, or 13 s after it sent the last
  renew that succeeded, so that it never acts on a lease another instance
  may hold already. The leader's bulk transactions take a share lock on
  the lease row in their epoch first and touch at most 256 rows.
- **Loops.** While it leads, an instance runs the loops of every package,
  each on its interval and when `ferun_leader` wakes it, one run at a
  time; an error is logged and the loop runs again.

| Loop | Interval | Does |
|---|---|---|
| node expiry | 5 s | marks a node down whose telemetry is older than `--node-ttl`; only while the leader has led for `--node-ttl` and the database has not failed for as long |
| reconcile sweep | 60 s | reconciles every ready node the leader is connected to |
| join-token expiry, fleet retention | 60 s, 1 h | expired join tokens; old feosd starts, event gaps, node and pod certificates |
| mesh peers | 15 s | sends every node on the pods' mesh that the leader is connected to its peers ([The pods' mesh](#the-pods-mesh)) |
| down sweep | 5 s | sandboxes of down and removed nodes become `unknown` |
| lost grace | 30 s | `unknown` sandboxes of nodes down past `--lost-grace` become `hibernated`, `lost`, or `pending` when their pod was never seen |
| idle pause | 1 s | pauses running sandboxes idle for `--idle-pause`, unless a failed pause backs off |
| hibernate-after | 5 s | hibernates sandboxes paused for `--hibernate-after`, unless a failed hibernation backs off |
| drain | 5 s | moves sandboxes off draining nodes |
| tombstones | 10 s | deletes pods that belong to no sandbox |
| stale ops | 10 s | takes over or releases the ops of dead or stopping instances |
| unknown sweep | 15 s | syncs `unknown` sandboxes on connected nodes |
| repair | 30 s | repairs pods stuck pending or with an unreachable guest |
| pending re-description | 30 s | tells a sandbox waiting for capacity that there is some |
| process holds, expired holds | 60 s | releases holds of processes that ended, and expired leases |
| tenant deletion | 60 s | finishes deleting tenants |
| watch expiry | 60 s | deletes watches past their lease |
| stale instances | 60 s | deletes instance rows older than an hour |
| activity partitions | 1 h | keeps partitions for today and the next two days; drops those past retention |
| sign-in clean-up | 10 min | deletes stale sign-ins and expired UI sessions |

### Stopping

An instance stops on SIGTERM or SIGINT so that a rolling deploy cuts no
request, tool call or op:

1. Its row says stopping, `/readyz` fails, the leader resigns its lease.
   It begins no work of its own initiative any more, but still begins the
   ops that requests need: a request in the readiness window that needs a
   resume does not fail. From now on other instances may take its ops
   over.
2. It serves on for 5 s, so that balancers see `/readyz`, then stops
   accepting requests and waits for those in flight, until
   `--shutdown-grace`. The watches' event streams end at once, and their
   clients reconnect elsewhere; upgraded port connections (WebSockets)
   are closed once no request is left.
3. The engine refuses new ops ("instance <id> is stopping") and waits for
   its own, until the same deadline.
4. The ops still in flight are released: their sandboxes become `unknown`
   and another instance resolves them.
5. Only then the node connections close, the nodes' port stops, the row is
   deleted and the pool closed.

The process therefore takes up to `--shutdown-grace` plus a little to
end; whatever stops it must wait at least that long plus 30 s
([Operations](#operations)).

## The nodes' port

One TLS 1.3 listener, routed strictly by ALPN:

| ALPN | Client certificate | Goes to |
|---|---|---|
| `feos-node/1` | the node's | a node connection: the node is the HTTP/2 server of its `feos.*` APIs, the instance the gRPC client |
| `h2` | the node's | the uplink server: `feos.fleet.v1` without Join |
| `h2` | the node's, definitively refused | the refusal server: every call `PERMISSION_DENIED` |
| `h2` | none | the join server: Join alone |
| anything else, or none | | closed |

- **Accept.** Every TCP connection gets keepalive (15 s) and a
  `TCP_USER_TIMEOUT` of 20 s. At most 512 handshakes run at once, each with
  a 10 s deadline. An EOF before the ClientHello is a balancer's probe and
  is not logged; failures a remote party can cause are logged at most once
  a second per kind, with a count of the suppressed.
- **The server certificate.** Each instance issues its own from the
  issuer at its start: ECDSA P-256, 24 h, for `--node-server-names` and
  the URI `spiffe://<trust domain>/ferun`, issued again at three quarters
  of its life. The chain it shows ends with the root, so that a joining
  node can check it against its token's pin. Session tickets are off:
  every connection presents its current certificate.
- **The node's identity** is its certificate's one URI,
  `spiffe://<trust domain>/node/<name>`, never what it sends. The
  certificate is checked against the database at accept and on every
  re-check, never from a cache (at most 32 checks at once; a check waits
  at most 5 s for a slot): its serial must exist and not be revoked, the
  node neither removed nor forked. A certificate that passes at accept
  has its presentation recorded ([certificate lineage](#nodes)) in the
  same slot, so that a node that redials as fast as it can never has more
  database work under way at accept than the 32 slots. A definitive refusal
  (unknown serial, revoked, removed, forked) refuses the connection; any
  other error, such as a database failover, refuses a new connection but
  never closes one that is open: its re-check is retried with backoff. A
  connection is closed when its certificate expires.
- **Node connections.** The instance dials gRPC over the TLS connection
  it accepted, with keepalive pings every 10 s (5 s timeout) also without
  streams, no idle timeout and no service config, and registers it only
  once it is READY (within 10 s). A node may have several connections to
  one instance, after a renewal or a reconnect: the **current** one, the
  READY connection with the newest certificate, takes every new call;
  the others **drain**: their calls finish, and they are closed when idle
  or after `--conn-drain-timeout`, but never while an op's lifecycle call
  runs on them. A current connection that leaves READY (a GOAWAY, a dead
  transport) drains like the others.
- **Calls to a node.** `Conn` returns the current connection; without one
  it waits up to `--node-wait`, or up to the node's rollout deadline when
  it announced a restart, then fails with `UNAVAILABLE` "node <n> is not
  connected to instance <i>". The rollout deadline is the grace the
  database measures as left (`stopping_until - now()`) when the instance
  reloads it, counted from then on the instance's monotonic clock: an
  instance whose clock is off the database's waits as long as one whose
  clock is not. A lost connection is what tells an instance to fail calls
  for the node fast; it changes nothing in the database. A call into a
  pod's guest carries `feos-pod-id`.
- **Stream budgets**, per connection, of `--node-max-streams` N: long
  streams (exec, watch, port forward, console, traffic records, host
  streams) at most N/2, unary tool calls at most N/4; beyond them a call
  fails at once with `RESOURCE_EXHAUSTED`. Lifecycle calls, `GetPod`,
  `ListPods`, `DeletePod` and the ops' pod event streams are not counted,
  and always find the last quarter free. Per sandbox and instance: 32
  forward streams (a request waits up to 30 s for one), 16 watch streams,
  64 exec streams.
- **The uplink server** takes at most 4 uplinks per node; it allows
  pings every 5 s without streams, 16 streams and 2 MiB messages per
  connection. An uplink whose certificate was superseded takes no new
  calls and is closed once idle.
- **The join and refusal servers** share one limit of 64 connections, each
  closed after 60 s. The refusal server tells a removed, revoked or forked
  node that it has to join again: "node <n> was removed; join it again",
  "certificate of node <n> was revoked (<reason>); join it again",
  "identity of node <n> forked; join it again with a token pinned to its
  name".
- **Re-checks.** `ferun_node` makes every instance re-check the node's
  connections and reload its rollout deadline; after the listener
  reconnected, every node's, spread over 5 s.

## The CA

The platform CA ([ADR 0014](../adr/0014-identity-and-credentials.md)) has
one root, kept offline, and one intermediate, the **issuer**, which every
instance holds in `--ca-dir`. The issuer is the only CA key on an
instance: it signs node and pod certificates and the instances' own
certificates on the nodes' port. With the database's credential and the
at-rest key of the bindings, it is one of the control plane's three
secrets.

- **`ferund init-ca`** makes both. The root: ECDSA P-256, 10 years, path
  length 1, written to `--root-dir` (`root.crt`, `root.key`). The issuer:
  ECDSA P-256, 5 years, path length 0, written to `--dir` with
  `bundle.crt` (the root) and `issuer.json` (the control-plane domain,
  `ferun.<first --dns-domain>` by default). Both carry the same critical
  name constraints: DNS names only under `--dns-domain`, no IP address,
  URIs only in the trust domain. The two directories must differ, and no
  file is ever overwritten.
- **At start**, `serve` verifies the issuer's chain to a root of the
  bundle, requires the control-plane domain to lie within the issuer's
  DNS domains and every `--node-server-names` under it, and refuses to
  start when `root.key` is in `--ca-dir`.
- **Separating pods from the control plane.** The name constraints keep
  every certificate inside the platform's domains and away from IP
  addresses, but the control-plane domain lies beneath the same platform
  domain, and a URI constraint binds only the trust domain, never the
  path (Go's verifier enforces URI constraints; rustls-webpki ignores
  them). The separation is therefore in software, on two lines: issuance
  (a pod never gets a wildcard, a name in the control-plane domain or the
  control plane's URI; a node gets only its URI; only the instances'
  server certificates carry `spiffe://<trust domain>/ferun`), and the node,
  which requires that URI on the control plane's certificate beside the
  DNS name it was configured with. A pod's certificate of the same issuer
  fails that check. A second, server-only intermediate would make the
  split a name constraint, at the price of a second CA key on every
  instance.
- **Issuance.** Serials of 128 random bits; `NotBefore` ten minutes back,
  since nodes may boot before their clock is set; never an IP address,
  never a URI outside the trust domain; a signing request's own names are
  ignored. Nodes: client auth, `spiffe://<td>/node/<name>`, Ed25519 or
  ECDSA P-256, `--node-cert-ttl`. Pods: client and server auth, the
  sandbox's SPIFFE ID `spiffe://<td>/tenant/<tenant>/sandbox/<id>`,
  Ed25519, `--pod-cert-ttl`. Each says when its holder renews it
  (`renew_after`): a node's at half its lifetime, so that a node switched
  off at any time keeps at least half of it; a pod's at three quarters.
- **Join tokens.** A token is 32 random bytes. The node sends
  `feos-join-<secret>`; the operator gets `feos-join-<secret>.<pin>`, where
  the pin is the SHA-256 of the root that anchors the issuer (its
  SubjectPublicKeyInfo). The database keeps only the hash of what the node
  sends. A token may be pinned to a node name. The node checks, before it
  sends the token, that the server's chain holds the pinned root, names
  the configured DNS name and carries the control plane's URI, and takes
  the join's answer only if its trust bundle holds that root.
- **Sealed bindings.** A pod's bindings travel sealed to its run's HPKE
  key: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, ChaCha20-Poly1305, info
  `feos bindings v1`, the pod's ID and the bindings' version as associated
  data.

## Nodes

`fleet` serves `feos.fleet.v1`, the control plane's API for nodes. A node's
row is never deleted: removal is a status. Its statuses: `joined`, `ready`
(it reports), `down` (its telemetry aged past `--node-ttl`), `removed`.

- **Join.** The token's form, the name (a DNS label) and the signing
  request (at most 16 KiB) are checked first; at most 10 joins a second,
  20 in a burst, and 4 at once in the database, per instance. Then, in one
  transaction: an unknown token is refused, an expired one too. An
  unpinned token creates a new name only: a name that exists, whatever
  its status, gets `ALREADY_EXISTS` "node <name> exists; join it with a
  token pinned to its name". A pinned token is the operator's intent to
  join that name again, for a node that lost its key: it revokes every
  earlier certificate of the name (`rejoined`), clears a removal and a
  fork, and the old connections close on every instance. A used token is
  answered again only when the same key repeats it within 10 minutes and
  the certificate of its first answer is still the node's and not revoked,
  the node neither removed nor forked: the first answer was lost.
  Otherwise "join token already used".
- **Renewal.** `RenewCertificate` checks the presenting certificate in its
  transaction as at accept, issues for the request's key, which may be
  new, and records the predecessor. `GET
  /api/v1/admin/nodes/{node}/certs` shows the history.
- **Certificate lineage.** When a newer certificate of a node is first
  presented, every older one is revoked one superseded grace ahead,
  `--conn-drain-timeout` plus 10 minutes: the older connections take no
  new calls and drain, and are closed at that time at the latest. A node
  that renewed dials new connections with its new certificate at once. A
  superseded certificate presented in a new handshake after its grace
  means two holders use the node's identity: the node is marked forked,
  all its certificates are revoked, and it must join again with a pinned
  token. An old connection of the node's own, re-checked after the grace,
  is only closed. Almost every accept presents a certificate seen before
  and not newer than the current one: one read without a lock tells so,
  and nothing is written. A presentation that changes the lineage is
  recorded under the node's row lock, at most 4 at once per instance.
- **Instances for nodes.** Join and every telemetry answer list the
  instances with a `--node-advertise-address` whose row is younger than
  15 s, stopping ones included: a stopping instance stays listed until its
  last step, so nodes keep their connections while it finishes. A node
  keeps a node connection to each, and dials the name it was configured
  with only to join and while the list is empty.
- **Telemetry**, every 5 s, is the heartbeat: the receiving instance
  writes the database's time into `node_telemetry`, the figures beside it,
  and the node's static facts (CPU, memory and disk totals, versions, the
  object store and its ID, the pods' prefix) into its row when they
  change. Strings are sanitised, too-large numbers clamped. A `joined` or
  `down` node becomes `ready`. A new feosd start clears an announced
  restart and requests a reconcile.
- **Inventory hash.** The node reports a hash of its running and suspended
  pods; the registry's hash of the node uses the same formula over the
  sandboxes it holds there. A mismatch that persists for 10 s requests a
  reconcile, unless the last reconcile saw the same node hash already (a
  pod ferund does not own, made with `feosctl`, keeps the hashes apart
  for good).
- **Reconcile requests** (a new node connection, a persistent mismatch, a
  new start, a pod event for a sandbox the registry has on another node, a
  sync that cannot reach the node, the listener's reconnect) are claimed
  in the node's row at most every 30 s, by an instance connected to the
  node. An instance that is not sends `ferun_reconcile`.
- **Events.** `PublishEvents` takes batches of at most 512 events and
  1 MiB, numbered densely per feosd start. A malformed batch ends the
  stream with `INVALID_ARGUMENT`. Content is sanitised, never refused: an
  audit record's JSON that does not parse is kept as text, and an event
  this ferund does not know (from a newer feosd) is stored as an `unknown
  event` and acknowledged. In one transaction, under the lock of the
  start's row, events already stored are dropped, the rest stored; a
  number neither stored nor counted as dropped by the node is recorded as
  a gap. Only then the acknowledgement goes out; a storage failure ends
  the stream with `UNAVAILABLE` and acknowledges nothing. Pod events go to
  `activity`, audit records too (source `authn`), traffic records to
  `traffic_records`, node events (`started`, `stopping`) to `activity`; a
  `stopping` sets the node's rollout deadline, `--rollout-grace` ahead
  on the database clock, during which calls wait for it ([calls to a
  node](#the-nodes-port)). A record about a pod is attributed to its
  sandbox only when the pod is or was on that node; otherwise it is kept
  for the node's audit as foreign, and a partial index of the foreign
  records serves the node's activity a page at a time.
- **Pods' credentials.** `IssuePodCredentials` issues a pod's certificate
  when the sandbox is placed on the asking node and has a run there
  (creating, running, resuming), at most 5 per pod per minute
  (`RESOURCE_EXHAUSTED` beyond), and seals the sandbox's bindings to the
  run's HPKE key. A request repeated with the same key is answered again.
- **Liveness.** A node is marked down by the leader's node expiry, never
  on one missed report, and only while the leader is healthy: after an
  outage of the control plane or a database failover, nodes have a full
  TTL to report again. Keep `--node-ttl` at least three telemetry
  intervals plus a failover.

### The pods' mesh

Where the network routes no prefix to the nodes, their pods' IPv6 goes
over a WireGuard mesh between them
([ADR 0017](../adr/0017-pod-mesh.md)); the node's side is in
[node.md](node.md). The control plane gives every node its /64 and sends
every node the others as its peers.

- **The ULA.** `--pod-network mesh` draws a /48, `fd` and a Global ID of
  40 random bits, when the database has none, and keeps it in
  `pod_network`; `--pod-network fdXX:XXXX:XXXX::/48` keeps that one. Every
  later start keeps the database's: an instance whose `--pod-network` is
  empty while the database is meshed, or names another /48, does not
  start ("the database's cluster is meshed on …"). A cluster does not
  move between a routed network and a mesh, nor between meshes.
- **`JoinMesh`**, on the uplink, takes the node's name from its
  certificate, which must pass the checks of every uplink call; an unknown
  or removed node gets `PERMISSION_DENIED`. In a routed cluster the answer
  is empty, and a request without a key, from a node with a prefix of its
  own, gets the ULA alone. Otherwise the key must be 32 bytes and the
  endpoint an IP address and a port, an IPv6 address in brackets, neither
  unspecified, multicast, IPv4-mapped nor with a zone
  (`INVALID_ARGUMENT`). Under the node's row lock, a node without a
  subnet ID gets the next of the sequence `mesh_subnets`, 1 to 65535,
  which never hands one out twice (`RESOURCE_EXHAUSTED` once it ends), and
  its /64 is the ULA with the ID in bits 48 to 63; the same node gets the
  same one on every call. The key and the endpoint are stored beside it,
  in `nodes`.
- **The peers' version.** Every change of who the peers are counts up
  `pod_network.peers_version` in its transaction and sends the hint
  `mesh-peers` on `ferun_leader`: a node's first subnet, a new key or
  endpoint, a peer's removal, and a pinned re-join, which takes the node
  off the mesh until it asks again, for a new /64. A removed node keeps
  its subnet ID, which is never given to another. Down nodes stay peers.
- **Distribution.** The leader's loop `mesh-peers` reads the peers and
  their version in one statement, every 15 s and when the hint or a
  node's first connection to the leader wakes it, and calls
  `feos.network.v1.MeshService` on the node connection of every connected
  node that is a peer and has not taken this version: `PutMeshPeers` with
  the others whole, after the node's connection came up and in a new term,
  or `UpdateMeshPeers`, the peers added, changed and removed since the
  version it took in this term; a delta refused with
  `FAILED_PRECONDITION` is followed by the peers whole. At most 16 calls
  run at once, each for at most 10 s, never in a transaction; a failure
  is logged and the node is tried again on the next run. A feosd that
  answers `UNIMPLEMENTED` is logged once and not asked again on its
  connection. What the leader sent lives in its memory only: a node's last
  connection to it gone, it is forgotten.
- **The admin API** shows a node's /64 and endpoint as `meshPrefix` and
  `meshEndpoint`.

## Sandboxes

A sandbox is a row in `sandboxes`; its pod on a node has the sandbox's ID
as its pod ID. The row is the truth every instance acts on; nothing about
a sandbox lives in an instance.

### The pod and its incarnation

Every row has an **incarnation**, a UUID set when the row is inserted: a
sandbox deleted and created again with the same ID is a new incarnation.
The pod's spec is `{resources: {vcpus, memory_mib}, containers: [{name:
"main", image_ref, command: ["sleep", "infinity"]}], network: {}, labels:
{ferun.sandbox, ferun.tenant, ferun.incarnation}}`, the same on every
retry: feosd compares specs, labels included, so a create against a
leftover pod of an earlier incarnation fails instead of returning it. A
pod is **ours** when its incarnation label is the row's; ferund never
touches a pod without `ferun.sandbox`, and never adopts the state of a pod
that is not ours.

### States

`clientState`, as FeCode reads it:

| State | Booked | Means |
|---|---|---|
| `pending` | no | no pod yet, a create failed before the pod existed, or its node went down before its pod was ever seen |
| `creating` | yes | a create, start, restore or repair runs, or the pod was seen pending |
| `running` | yes | the pod runs |
| `pausing` | yes | a suspend runs |
| `paused` | no | the pod is suspended on its node |
| `resuming` | yes | a resume runs |
| `hibernating` | as before | a hibernation runs |
| `hibernated` | no | in the object store; brought back with a restore, on any node of the same store |
| `stopped`, `failed` | no | the pod stopped or failed; the description says why |
| `unknown` | as before | the truth is not known: the node is down, or an op was released; a sync or the lost grace resolves it |
| `lost` | no | the pod that ran is gone and there is no checkpoint, or it went down with its node; until deleted, unless its node returns with the pod |

**Bookings follow resource use.** Every write of a state books the pod's
size on its node or frees it, as the table says. An op's admission books
through the gates; a pod that is found running where the gates would
refuse it is booked anyway, as overbooked, with an activity row: reality
wins over the books. Lock order everywhere: tenant, sandbox, at most one
node.

### Ops

Every lifecycle change is an op: begun in one transaction under the row's
lock, which writes the op, its owner, target and deadline and the
transitional state; run against the node outside any transaction; and
finished in another transaction that applies what the node did.

| Op | From | During | Calls | Deadline |
|---|---|---|---|---|
| create | pending | creating | CreatePod | 7 min 5 s |
| start | stopped, failed | creating | StartPod | 7 min 5 s |
| resume | paused; hibernating, in the place of a hibernate that a resume abandons | resuming | ResumePod | 7 min 5 s + 8 s/GiB |
| restore | hibernated | creating | RestorePod of the checkpoint | 7 min 5 s + 8 s/GiB |
| pause | running | pausing | SuspendPod | 2 min + 4 s/GiB |
| hibernate | paused; stopped, failed and running when draining | hibernating | HibernatePod | `--hibernate-timeout` + 16 s/GiB |
| repair | creating without an op, running with an unreachable guest | creating | StopPod and StartPod; for a pod stuck in its restore, DeletePod and RestorePod | 7 min 5 s + 8 s/GiB |

GiB is the sandbox's memory, rounded up. A hibernation uploads the pod's
disk and memory to the object store and takes minutes per GiB, hence its
own timeout. A feosd that reports its hibernations ([Follow-ups for
feosd](#follow-ups-for-feosd), item 2) runs one in a task of its own,
which a `HibernatePod` that comes meanwhile joins and which a cancelled
call or a dropped connection never stops, so the timeout is how long
ferund waits: past it the pod is still suspended, the sandbox `paused`
("hibernation failed: …"), and the hibernation's end arrives as the
pod's event, unless a resume abandons it first. A hibernate released by
a stopping instance goes on the same way, and the next Ensure resumes
the sandbox. An older feosd runs the hibernation in its call, which the
deadline, or a stopping instance that drops it, cuts at an arbitrary
point: there the timeout is how long a hibernation may run. A resume
has a restore's deadline: a `ResumePod` that meets a hibernation it
cannot abandon waits for it, then restores the pod from the new
checkpoint inside the call.

- **Detached.** An op's node calls never run on its caller's context:
  they keep the caller's values but not its cancellation, and end at the
  op's deadline. A client hanging up, an MCP cancellation or the end of a
  leader term never cuts them, because feosd stops a mutating pod call
  (other than `HibernatePod` on a feosd that reports its hibernations) at
  an arbitrary point when its request is cancelled. Callers wait on the
  op and may stop waiting; the op goes on. An instance runs one op per
  sandbox at a time, except that a hibernate a resume preempted runs on,
  fenced out, until its call answers.
- **Before begin.** An op on the node the sandbox is on (start, resume,
  pause, hibernate, repair, a takeover) first waits for this instance's
  connection to that node, up to `--node-wait` or the node's rollout
  deadline, and sends nothing until then. A sandbox paused on a node that
  restarts within `--rollout-grace` thus resumes once the node is back.
  The leader's loops never wait: they consider connected nodes only.
- **Begin.** The row is locked by its ID. An op in flight of another
  instance is taken over only when that instance is not live (dead or
  stopping): the takeover runs the same op again with a new ID and
  deadline, since every call is idempotent or answered from the pod's
  state. An idle pause and a hibernate-after recheck, in later statements,
  that the sandbox is still idle, holds nothing and is not backed off
  ([Policies](#policies)); a pause for a drain only that nothing executes;
  an explicit pause nothing. An admission locks the target node's row,
  rechecks the gates and books. The begin's transaction runs on a context
  of its own, bounded by 30 s, never its caller's: a client that hangs up,
  or a leader term that ends, while the COMMIT is on its way would leave
  the row in an op that nobody runs. A COMMIT whose answer is lost all the
  same (the connection broke) is judged by the row: an op that committed
  runs.
- **Preempt.** A hibernate of a paused sandbox that a resume abandons now
  is replaced by a resume, in one transaction under the row's lock: the
  resume gets a new op ID, this instance as its owner and its own
  deadline, and is admitted and booked as any resume. Its `ResumePod`
  makes feosd abandon the hibernation, which stops at its next step and
  answers `ABORTED`, and the pod resumes on its node from its snapshot;
  what was uploaded stays in the object store, named by no head. Whoever
  owned the hibernate, its op is fenced out by its ID: its finish writes
  nothing but a newer checkpoint, and its activity ends "abandoned".
  Every `HibernatePod` carries its op's ID as `request_id`, and feosd
  reports a hibernation in flight in `PodInfo.hibernation`: its phase,
  whether a resume abandons it now, and the request IDs of the calls in
  it. Only Ensure preempts, and only a hibernation that its op's own call
  has reached; feosd would otherwise start that call's hibernation anew
  behind the resume, and answers `ABORTED` to a late call of a
  hibernation it abandoned. A drain's hibernate, Ensure's off a draining
  node and a takeover's on a draining node send `no_abandon`, which feosd
  keeps for the hibernation's whole run: a resume waits for these, and
  the admission refuses a resume on a draining node anyway. A preempting
  resume that reaches no connection leaves `unknown`, "node <n> is not
  connected to instance <i>; the hibernate the resume was to abandon goes
  on", for a sync to resolve.
- **Call and settle.** After the call, also after its error, the op reads
  the pod and waits while it is pending, on its event stream with a read
  every 5 s, until the deadline. `UNAVAILABLE` and `RESOURCE_EXHAUSTED`
  while waiting are transient. Call errors are never mapped to a state
  directly: the pod read after them decides.
- **Finish** is retried until it commits. An op that is no longer the
  row's (taken over, preempted, or the row deleted or re-created
  meanwhile) writes only a tombstone for a pod it may have made, or a
  newer checkpoint into the row on its node, in any state. A
  rollback, when this run reached no connection and sent nothing, puts
  the row back as the begin found it; after a takeover, which cannot know
  what the first owner sent, it leaves `unknown`.

| Observed on the target | Becomes |
|---|---|
| ours, running | `running`; a run opens, restored from the checkpoint when a resume brought the pod back from a newer one; a failed pause lands here too |
| ours, pending at the deadline | `creating`, "still starting on node <n> after <d>" |
| ours, suspended | `paused`; a failed hibernation lands here too, and one that a resume from elsewhere abandoned ("hibernation abandoned for a resume") |
| ours, hibernated | `hibernated`, with the checkpoint (never an older one) |
| ours, stopped or failed | `stopped` or `failed`, with the pod's reason |
| not ours | a tombstone for that pod, then as not found |
| not found | `hibernated` without a node when there is a checkpoint; `lost` when the pod existed; else `pending`, and a node where the create failed is avoided next time |
| no answer until the deadline | `unknown`, "outcome unknown"; a sync resolves it |

A newer checkpoint that our pod reports is taken in every state, here and
by Sync, never an older one. A repair that restarts a pod, and a start,
resume or restore that came up with a cold boot, end the sandbox's process
holds; a repair that found the pod healthy and made no call ends nothing.

**Stale ops.** The leader takes over the op of an instance that died or
stops, when it is connected to the op's node; otherwise it releases the
op and the sandbox becomes `unknown`. An op of a live instance is never
taken over by another; one more than 5 minutes past its deadline is
released and logged as an error. An op that the row says is an
instance's own and that the instance does not run is orphaned (its begin
committed, but nothing ran it): Ensure on that instance takes it over at
once, as a dead owner's, and every instance looks for its orphans every
10 s, taking over those on nodes it is connected to and releasing the
others. That look reads all of the instance's ops but those it runs, 64
at a time in deadline order, so an orphaned hibernate, whose deadline
sorts last, is found however many ops the instance runs.

### Ensure

Before every tool call, port forward and explicit resume, `Ensure` brings
the sandbox to running, within the preparation budget of 7 min 5 s, on
any instance: it reads the row and starts or joins what its state needs.
A pending sandbox gets its image resolved and is placed and created; a
paused one resumed on its node, or hibernated and restored elsewhere when
its node has no room or drains; a hibernated one placed and restored; a
stopped or failed one started; an `unknown` one synced. A running sandbox
on a draining node is moved first. A `lost` one is synced once, in case
its node came back with the pod, then "sandbox lost". A hibernate in
flight that a resume abandons now is preempted ([Ops](#ops)), and the
sandbox resumes on its node; while the node's answer may still change (no
hibernation reported yet, the op's call not in it yet, no answer), Ensure
asks again every second. Any other hibernation in flight (a drain's, one
past its point of no return, every one on a feosd that does not report
its hibernations) is waited for at most 30 s; after that the caller gets
"sandbox <id> is hibernating; retry in a minute", and a later call
restores the sandbox. Ensure starts at most one op of each kind per node
per call, and returns the error of an op that failed instead of trying
again: "<op> on node <n> failed: <description>". Every error of Ensure
reaches the client as `ensure sandbox: <error>`.

`Acquire` is Ensure and a **hold**: a row in `sandbox_holds` that keeps
the sandbox from being paused while a call or a forward runs, leased for
30 s and renewed every 10 s by its instance, taken under a lock that
excludes a pause's begin. Holds of processes started with `process_start`
last until the process ends or its timeout passes; a timeout of 0 holds
the sandbox until the process is stopped or found ended.

### Placement

Gates, in order, each with the reason that the description and the
placement audit (the placement's activity row, with every node's
verdict) show: the node is `ready`; not draining; not forked;
connected to this instance; no tombstone of this pod ID on it; not
avoided after a failed create; no other sandbox on it whose ID starts with
the same 8 hex digits and may hold a TAP device (feosd derives the TAP
name from them); for a restore, the checkpoint's object store; slots;
CPU, `floor(cores × cpu_overcommit)`, not gated when 0; memory, the total
less the headroom; free memory, the reported available less the headroom;
disk, the available against `--disk-budget`. Among the nodes that pass,
`binpack` takes the one whose scarcest resource has the least free share,
`spread` the most, ties by name; a hibernated sandbox prefers the node
that holds its record. The op's begin rechecks the gates under the
node's lock, so two instances placing at once never overbook a node.
Nothing fits: the sandbox stays, "no capacity (retryable): no feasible
node; n1: <reason>; ...". Slots (50), headroom (1 GiB) and overcommit (16)
are per node, set with `PATCH /api/v1/admin/nodes/{node}`.

### Images

A sandbox's image is resolved once, before its first create, to
`<registry>/<repository>@sha256:<digest>` (Docker Hub names normalised,
`linux/amd64` from an index, anonymous registry tokens, a tag cache of
60 s), and pinned in the row: every retry and every later create sends the
same digest, so a moved tag never changes a sandbox. Private registries
are not supported. The tenant names the image, and the image names the
registry, so the resolver reaches registries as bindings reach servers:
over https only, without a proxy, and at IPv4 addresses outside the deny
set (`--deny-prefix`, by default the private, shared, link-local,
loopback, "this network", multicast and reserved ranges), judged on the
address it dials, for the registry, every redirect and the token realm
alike. Its errors
name the address refused, or that the registry is unreachable, never
what a dial found there.

### Sync and reconcile

- **Sync** applies one pod's state to its row, after a pod event or when
  a caller found the pod not where or not as expected. While an op runs,
  the sync is deferred until it ends, never dropped. It reads the pod on
  the row's node and applies it under the row's lock, if nothing changed
  in between. An instance not connected to the node hands the sandbox to
  one that is, through a reconcile request.
- **Reconcile** applies a node's whole inventory: it takes a snapshot of
  the node's rows first, then lists the pods, and acts only on rows that
  did not change since the snapshot. A pod of ferund's without a row, or
  whose row is elsewhere or of another incarnation, gets a tombstone. A
  row whose pod is listed takes its state, including a `hibernated` or
  `lost` row whose node came back with the pod: the pod is adopted and
  booked again. A row whose pod is missing becomes what a sync makes of
  not found.

### Tombstones

Every decision that a pod is to go writes a tombstone (pod, node,
incarnation) in the deciding transaction: a delete, a restore elsewhere,
a stale record, an orphan, a fenced-out op. The tombstone is processed by
an instance connected to the node: it reads the pod and deletes it only
when its incarnation is the tombstone's (or it has none), and drops the
tombstone only after the node answered. Processing always runs detached,
like an op's calls. While a tombstone exists, placement puts no pod of
that ID on that node. Tombstones of removed nodes are dropped.

### Policies

The down sweep marks the sandboxes of down and removed nodes `unknown`,
leaving those with an op in flight to the op. After `--lost-grace` they
become `hibernated` (with a checkpoint, restored elsewhere on the next
use), `lost` (their pod was seen there), or `pending` without a node when
their pod was never seen there, as after a first create whose outcome
the node took down with it: nothing ran, so nothing is lost, and the
next use creates the sandbox. No tombstone is written, since the pod may
still run: a returning node's pod is adopted, or tombstoned by reconcile
when its sandbox is pending or placed elsewhere by then. Idle pause,
hibernate-after, drain and repair begin ops as their triggers. A drain
hibernates what is paused, and pauses and hibernates what runs without
execs; a forward does not keep a sandbox, since the port door closes its
upgraded connections, a preview's WebSocket among them, once the sandbox
leaves running, and their clients reconnect to where it runs next. A call
for a sandbox on a draining node starts nothing there: it waits until the
sandbox has moved. After `--drain-timeout`, minutes after the drain's
start, the execs that still run are cancelled and the rest moves
regardless. A sandbox that ran is restored on another node as soon as its
hibernation is done, where the calls that waited go on; without room
elsewhere it stays hibernated until its next use. A resume never abandons
a drain's hibernation. A node without an object store cannot be drained.

An idle pause that leaves the pod running, and a hibernate-after that
leaves it suspended, back their own policy off, so that a node that
cannot write a snapshot, or an object store that refuses uploads, is not
asked again on every pass: the row names the policy (`policy_failed`),
and that policy's loop, and the recheck of its begin, begin no op for the
sandbox before `policy_not_before`, 30 s after the first failure and
doubling per consecutive one of the same policy up to 30 min. The other
policy is not held back: a sandbox whose hibernation failed, resumed
since, is paused when it idles, and its idle pause's failures count from
one. A pause that ends paused and a hibernation that ends hibernated,
whatever began them, and a move to another node clear the backoff. Drain
and repair do not back off.

### Delete

`DELETE /api/v1/sandboxes/{id}` removes the row in one transaction, with
a tombstone for every node a pod of this incarnation may be on (one that a
live op may still create waits for the op's deadline), and its tokens,
holds, watches, bindings and booking with it. An op in flight is fenced
out when it finishes. A create of the same ID right after is a new
incarnation, which placement keeps off a node until that node's
tombstone is done.

## Front doors

### REST

The routes, bodies, status codes and texts are the sandbox system's;
`go/internal/api` holds the route table, with each route's auth class:

| Class | Routes | Credential |
|---|---|---|
| admin | `/api/v1/admin/…` | an admin's UI session (cookie), or Basic with `--admin-users` |
| tenant | `/api/v1/sandboxes`, `/api/v1/sandboxes/{id}` (create, list, get, delete, pause, resume, tokens, access), `/api/v1/tools` | a tenant's token |
| sandbox | `/api/v1/sandboxes/{id}/…`: exec, files, dirs, search, fetch, processes, watches, bindings | the sandbox's own token |
| mcp | `/mcp` | the sandbox's token, with the door's own answers |
| forward | `/fwd/…`, `/api/v1/sandboxes/{id}/ports/…` | the sandbox's token in `Authorization` alone ([ports](#ports)) |
| public | `/healthz`, `/readyz`, `/auth/…`, the UI | none |

A tenant's token is not accepted on a sandbox route, not even for its own
sandbox (the forward class answers it 401 "forwarding needs a sandbox
token", [ports](#ports)); whoever names what they may not see gets the
answer of an absent thing, 404. Every unknown route under `/api/` is 404
`{"error": "no such route: METHOD PATH"}`, never 405. A sandbox ID must be
a UUID. The admin API has tenants and their tokens (hashed, shown once),
nodes (with their connections to the serving instance, certificates,
drain, removal and capacity settings), join tokens, instances and the leader, and the
activity of a sandbox or a node, with gaps and traffic records. `GET
/api/v1/admin/nodes` answers the nodes and `defaults`, the serving
instance's settings that the UI explains the fleet with:
`telemetryIntervalSeconds`, `nodeTtlSeconds`, `lostGraceSeconds` and
`drainTimeoutSeconds` (its timings, in seconds), `defaultVcpus`,
`defaultMemoryMib` and `defaultImage` (what a sandbox created without
them gets), and `minVcpus`, `maxVcpus`, `minMemoryMib` and
`maxMemoryMib` (the bounds a create accepts). An undrain clears the text
the drain loop left in the node's status description ("draining: …",
"drained", "cannot drain: …"); another writer's text stays. Deleting
a tenant deletes its tokens at once, then its sandboxes in batches as a
sandbox delete does; the leader finishes what a request did not.
`/readyz` fails while the instance stops, and after its start until it is
connected to every ready node that reported since, at most 30 s, so that a
rolling deploy sends no requests to an instance no node has dialled yet.

### Human sign-in

People sign in to the UI and the admin API with OIDC (`--oidc-issuer`):
FeCode, FeRun and FeChat share one provider. ferund is a confidential
client (`client_secret_basic`), with the authorization code flow, PKCE,
`state` and `nonce`; the ID token's signature (RS256, ES256 or EdDSA, from
the provider's keys) and claims are checked with the standard library.
The keys are fetched again on an unknown key ID, at most once a minute,
counting the first fetch. A person whose `groups` claim holds
`--oidc-admin-group` gets a session in the database, so that a sign-in
begun on one instance completes on another and a sign-out ends it
everywhere; anyone else gets "no access" and no session. A request with
the session cookie that changes something must carry `X-Requested-With:
ferun` and, with an `Origin`, the public URL's; ferund sends no CORS
headers. Basic credentials, when present, are checked first. Without
OIDC and `--admin-users`, the admin routes answer 503. An admin route's
401 carries `WWW-Authenticate: Basic`, which makes a browser show its
login dialog, only with `--admin-users` and only to a request without
`X-Requested-With: ferun`: the UI sends that header on every request and
shows its own sign-in screen on a 401, so a session that expired does
not bring up the browser's dialog over it.

### Tools and MCP

The agent's tools are the sandbox system's twelve, with their
descriptions and schemas: `bash`, `read`, `write`, `edit`, `ls`, `rg`,
`fd`, `process_start`, `process_list`, `process_logs`, `process_stop`,
`webfetch`. A call acquires the sandbox, then runs on its node over this
instance's node connection: the file tools and `webfetch` as
`feos.tool.v1` calls in the container `main`, `bash` as an exec of `bash
-c` in `/`, the process tools as execs of small scripts that keep their
records in the container. Outputs are capped at 50,000 bytes with the old
markers; a call has 120 s by default and then reports `timeout after Ns`.
A call that finds the pod not running or not there syncs, acquires again
and retries once. REST and MCP share the one entry point:

- **REST** answers 200 whenever the tool ran, its exit code being its
  verdict, and 502 `{"error": "<tool>: <error>"}` otherwise, with
  `Retry-After: 5` when a retry is worth it (no capacity, a node or
  instance unavailable or stopping, a hibernation in flight, a budget).
- **MCP** (`/mcp`) runs the go-sdk's Streamable HTTP handler stateless,
  so that any instance serves any request: the bearer is resolved on
  every request. Its answers are the door's own, which the SDK's clients
  act on: 403 for a foreign Origin, 405 for a GET, 401 for a missing
  token or an unknown one on `initialize`, a JSON-RPC "session not found"
  (404) for an unknown token on any other request, so that a client
  starts a new session, and 503 when the token cannot be looked up. A
  session ID exists only for cancellation: it is signed with the cluster's
  key and verified by any instance, and a `notifications/cancelled` with
  it is broadcast (`ferun_cancel`) to whichever instance runs the call.
  While a call runs, progress goes out every 10 s and per output chunk,
  and an SSE comment every 25 s keeps balancers from closing the
  response.

### Ports

`/fwd/{id}/{port}/…` and `/api/v1/sandboxes/{id}/ports/{port}/…` forward
HTTP to a port inside the sandbox, with the sandbox's token in
`Authorization: Bearer` and nowhere else. Without it the door answers 401
"forwarding needs a sandbox token", and so it answers a tenant's token,
on both paths: the answer names no sandbox. An unknown token and another
sandbox's get 404 "no such sandbox". A sleeping sandbox is brought up
detached from the request, so that a browser that gives up on a cold
start does not cancel it. The request goes through a reverse proxy whose
transport dials `PortForwardService/Forward` on the sandbox's node
connection; the token stops at the door, and the guest sees the Host the
client sent, which for FeCode's previews is the preview host. Upgrades
such as WebSockets ride through. Connections are pooled per pod and port,
at most 32, for whose slots a request waits up to 30 s; a forward holds
the sandbox awake. A failed dial is read by its code and feosd's text,
since both failures that matter come as `UNAVAILABLE`: "127.0.0.1:<port>:
…", the guest's, means nothing listens on the port (502), "the guest of
pod …" that the guest does not answer (503), anything else that the node
connection failed (503). A refused Kubernetes listener is 403, a pod that
does not run yet 503 with a retry. A feosd that does not forward this
service for pods answers `UNIMPLEMENTED`, and the door 502 "node <n> does
not forward ports yet" ([follow-ups](#follow-ups-for-feosd)).

### Watches

`POST`, `PUT`, `GET …/events` (SSE) and `DELETE` on
`/api/v1/sandboxes/{id}/watches` keep the sandbox system's contract: a
PUT carries the whole path set and doubles as the heartbeat, and `stale`
means "read these paths again". A watch is a row, so any instance answers
it, with one **owner**: the instance that holds the guest's watch stream
and the replay ring of the last 1024 events. An event stream opened on
another instance takes the watch over, and event IDs carry the owner's
epoch, so they keep increasing across owners. A PUT on another instance
writes the set and waits for the owner's answer. When the guest's stream
ends (a pause, a lost node connection, a feosd restart) the owner sends
`stale` and places the set again once the sandbox runs and its node is
connected. A watch never wakes a sandbox and never holds one awake.

### Kubernetes and SSH bindings

A sandbox's bindings ([ADR 0014](../adr/0014-identity-and-credentials.md))
are served only with `--bindings` and `--secret-key-file`. Off, the
default, their routes are not registered, so they answer 404 "no such
route", which FeCode reads as "no such surface on this platform", and
`GET …/access` answers `{"kube":null,"ssh":null,"denies":[]}`. On, they
are stored with every credential sealed at rest (AES-256-GCM under the
key, bound to the table, sandbox, name and kind), validated with the old
codes, checked with preflights that dial from the instance, and reach a
pod sealed to its run's key when its `feos-authn` asks for its
credentials; nothing is pushed to a node, so a change reaches a running
pod at its next start, resume or restore. `…/access` shows them with
their use from the pods' audit records. They stay off by default until
feosd delivers them ([follow-ups](#follow-ups-for-feosd)).

### The management UI

`web/` is a React app built with Vite into `go/internal/ui/dist`, which
ferund embeds; only `dist/.keep` is committed, so the module builds
without npm, and such a build answers the UI's routes with 404. It shows
tenants and their tokens, sandboxes with their activity, network and
access, nodes with their certificates and capacity, join tokens, and
instances with the leader, and signs people in through `/auth/`. A table
too wide for the window scrolls sideways in its own box; the nodes'
table fits a 1280 px wide window, and its memory bar fills by, and is
labelled with, the host's memory in use against its total. Its node
connections are those to the instance that answered, which with several
replicas need not be the one a node dialled; so it calls a node not
connected only when the node is down or removed. An instance is shown
live, stopping while it stops but still writes its row (it serves on),
or gone once its row is older than 15 s; the API's `live` and `fresh`
say which, on the database clock.

## The activity log

`activity` holds ferund's own rows (tool calls, ops, runs, state changes,
placements, credentials, sign-ins) and the nodes' (pod events, `feosd`
starts and stops, `feos-authn`'s audit records), as events and spans:
runs (`run-<uuid>`) are roots, an op is a child of its run. ferund's rows
are written in batches from a bounded queue that never blocks a request
and counts what it drops; rows that must commit with a lifecycle write go
in that transaction. `activity` and `traffic_records` are partitioned by
day and dropped after `--activity-retention` and `--traffic-retention`.

## The test node

`internal/testnode` speaks feosd's side of the wire: it joins with a
token after checking the pin, keeps a node connection and an uplink to
every listed instance, reports telemetry with the inventory hash,
publishes events with acknowledgements, renews its certificate, and asks
for pods' credentials. Its pods live in memory under feosd's rules,
hibernate into an object store in memory or in a directory that several
test nodes share, and have a fake guest: commands run in a directory per
pod, the tools work on it, watches answer from a script, ports dial local
addresses. It can restart, crash and be partitioned, and its mutating pod
calls can stop where today's feosd stops when their request is cancelled,
which proves that ferund never cancels them. Its hibernations run as
feosd's do: in a task of their own, which further calls join and a resume
abandons until the head is put; the script points `HibernateUpload` and
`HibernateHead` hold one in its upload or right after its point of no
return. With `ResumeWaitsForHibernation` it plays a feosd that does not
report its hibernations, whose resume waits for one. With `Mesh` it is on
the pods' mesh: it asks `JoinMesh` with a random key and its endpoint on
every new uplink and whenever its endpoint changes, and keeps the peers
that `MeshService` gets in memory; without, `MeshService` answers
`FAILED_PRECONDITION`. `ferun-testnode` runs one for development:

    ferun-testnode --join-token feos-join-<secret>.<pin> \
        --server-name ferun.example.com --addr 127.0.0.1:7443 \
        [--name NAME] [--trust-domain feos.local] [--state-dir DIR] \
        [--object-store-dir DIR] [--pending-delay 2s] \
        [--mesh [--mesh-endpoint ADDRESS:PORT]]

## Operations

### Deployment

One shape at every size: instances of `ferund`, PostgreSQL, and nodes
that dial the instances.

- **One machine.** The package `ferund` ([below](#the-package)), and
  PostgreSQL beside it without replicas; feosd on the same machine dials
  the control plane on the loopback with the same mTLS. Without
  `--node-advertise-address`, nodes reach the instance by the name they
  were configured with.
- **Several instances**, from the container image (`go/Dockerfile`,
  distroless, non-root, ports 8100 and 7443) or the package: an HTTPS
  balancer in front of the front doors, a TCP passthrough or DNS in front
  of the nodes' port, and every instance with a `--node-advertise-address`
  at which nodes reach that very instance. `deploy/k3s/ferun/` is the
  lab's deployment on k3s.

### Requirements

- **PostgreSQL 17 or newer.** With replicas it must replicate
  synchronously (`synchronous_commit = on` with `synchronous_standby_names`
  naming at least one standby, or `remote_apply`), so that a failover
  never loses a commit that ferund acknowledged: a booking, an op's
  begin, an event's acknowledgement. ferund sees a failover as a
  reconnect, after which it reconciles every node it is connected to.
- **Termination grace.** A stopping instance finishes its requests and
  ops for up to `--shutdown-grace` (10 minutes) before it closes its node
  connections. Whatever stops it waits at least that plus 30 s: the
  package's unit has `TimeoutStopSec=630`, and a container deployment sets
  its termination grace period (Kubernetes:
  `terminationGracePeriodSeconds`) to at least `--shutdown-grace` plus
  30 s. The default of 30 s kills ferund long before the tool calls in
  flight end. A longer or shorter `--shutdown-grace` moves both.
- **The front-door balancer:** an idle timeout of at least 60 s (MCP's
  keepalive comes every 25 s), no response buffering (MCP's answers, the
  watches' events and forwarded responses stream; the watches send
  `X-Accel-Buffering: no`, the rest needs the balancer configured so),
  long-lived responses (a tool call may run minutes, an event stream
  days), and no routing by Host for `/fwd/`: FeCode sets the Host of a
  forward to its preview host.
- **The nodes' balancer** passes TCP through; a proxy that terminates TLS
  or reads HTTP/2 breaks the node connection and the client
  certificates. Nodes connect to every instance listed to them, which is
  why every instance sets `--node-advertise-address`.

### The CA ceremony

1. On a machine kept offline, `ferund init-ca --root-dir ROOT --dir CA
   --dns-domain example.com --trust-domain run.example.com`: the root's key
   stays in `ROOT` and never reaches an instance.
2. `CA` (the issuer, `issuer.json` and `bundle.crt`) goes to every
   instance's `--ca-dir`. `init-ca` writes `issuer.key` with mode 0600 for
   whoever ran it; ferund must read it, and no one else. With the
   package: `chgrp ferun /etc/ferun/ca/issuer.key && chmod 0640
   /etc/ferun/ca/issuer.key`; in the container image, its user 65532 must
   read it, as a Kubernetes secret mounted with `fsGroup: 65532` gives.
3. A join token, from the UI, `POST /api/v1/admin/join-tokens`, or
   `ferund join-token`, goes to the node: feosd's `--join-token-file`, with
   `--control-plane`, `--control-plane-name` (a name of
   `--node-server-names`) and `--trust-domain`.

A node's certificate lives 7 days and is renewed at half of that, so a
node switched off at any time still has at least 3.5 days of it: an
outage of hours, or a node switched off for a weekend, needs nothing. A
node switched off for longer than half of `--node-cert-ttl` may come
back with its certificate expired, which the nodes' port refuses in the
handshake. A node whose certificate expired, that lost its key, or that
was removed joins again with a token pinned to its name. `bundle.crt`
may hold several roots, for a rotation; a join token's pin is always the
root that anchors the issuer.

### Moving from the sandbox system

- The front doors stay on port 8100, with the same paths and texts;
  FeCode's backend and its MCP client work unchanged.
- Tenant and sandbox tokens are stored as hashes: a token is shown once,
  when it is made, and tenants' tokens are listed by hash.
- The bindings are off by default.
- The SQLite registry is not imported: sandboxes are created again.

## The package

On Debian 13, `ferund` comes as a `.deb`, built by `make ferund-package`
into `target/packages/ferund/`, apart from the node's packages of `make
packages` ([node.md](node.md#packages)), so that a node image never holds
it, and published with them to https://apt.fecode.dev by `make
publish-apt`. `make test-ferund-package` installs it in a container and
checks what it leaves:

| Path | What it is |
|---|---|
| `/usr/bin/ferund` | static, with the UI embedded |
| `/usr/lib/systemd/system/ferund.service` | the unit |
| `/usr/lib/sysusers.d/ferund.conf` | the user `ferun` |
| `/etc/ferun/ferund.env` | the configuration, `FERUN_*` variables; mode 0640, group `ferun` |
| `/etc/ferun/ca/` | the issuer's directory, mode 0750, group `ferun`; empty |

`ferund.service` runs `ferund serve` as `ferun`, with
`EnvironmentFile=/etc/ferun/ferund.env` and `TimeoutStopSec=630`, the
default `--shutdown-grace` plus 30 s; a longer grace needs a drop-in with
a longer timeout. The unit has a read-only view of the system: ferund
writes nothing locally. Installing the package creates the user and
neither enables nor starts the unit: an instance needs its database and
the issuer first (`FERUN_DATABASE_URL`, the files from `init-ca` in
`/etc/ferun/ca`, `issuer.key` readable by the group `ferun`), then
`systemctl enable --now ferund`. The group `ferun` on `/etc/ferun/ca` and
`ferund.env` is a `dpkg-statoverride`, which the first install sets once
the user exists, so that dpkg keeps it on a reinstall's directory and on
a new `ferund.env` an upgrade brings. An upgrade restarts a running unit;
removing the package stops it; purging it disables it and drops the
statoverrides, and `/etc/ferun/ca` stays when it holds the issuer.

## Choices ADR 0015 leaves open

- **ALPN names.** `feos-node/1` for the node connection, `h2` for the
  uplink and the join.
- **MCP** runs stateless, with signed, self-describing session IDs used
  only for cancellation across instances.
- **Watches** have one owner instance each, which a stream opened
  elsewhere takes over in a new epoch.
- **The at-rest key** of the bindings is a third secret beside the issuer
  and the database's credential.
- **One issuer** signs nodes, pods and the instances' certificates on the
  nodes' port, keeping one CA key per instance; the control plane's
  identity is separated by issuance and by the node's check of its URI
  ([The CA](#the-ca)).
- **Join tokens carry a pin** of the root, so that a fresh node
  authenticates the control plane before it sends its secret.
- **Certificate lineage** supersedes a node's older certificates after a
  grace and treats an older one presented later as a forked identity.

## Follow-ups for feosd

What ferund does against today's feosd is described above; these are the
changes to feosd it relies on or improves with:

1. **The node side of ADR 0015** is built (`crates/fleet-client`,
   [node.md](node.md#the-control-plane)), except the event buffer on disk:
   today it is in memory, and a restart of feosd loses what was not
   acknowledged.
2. **Mutating pod calls in a task of their own**, which the request awaits,
   so that a cancelled call or a dropped connection never cuts them: the
   prerequisite of ADR 0015's "retry on `UNAVAILABLE`". Built for
   `HibernatePod` ([ADR 0008](../adr/0008-suspend-and-restore.md)),
   together with `PodInfo.hibernation` and the abandon by `ResumePod` that
   the preempt relies on; a feosd without it reports no hibernation, and
   Ensure waits for its hibernates as before. The other calls still run in
   their request: ferund never cancels them and repairs stuck pods.
3. **Port forwarding for pods** is built: feosd forwards
   `feos.portforward.v1.PortForwardService` to a pod's guest. The codes
   stay as they are, both "nothing listens" and "the guest is unreachable"
   `UNAVAILABLE`; the texts tell them apart, and `forward.rs` documents
   them as the contract ([Ports](#ports)). Against a feosd from before,
   which answers `UNIMPLEMENTED`, ports answer 502.
4. **Idempotent `SuspendPod` and `ResumePod`**; ferund resolves their
   `FAILED_PRECONDITION` by reading the pod meanwhile.
5. **Bindings for a running pod**: an RPC that sets a running pod's sealed
   bindings, and `feos-authn` itself; then `--bindings` can default to on.
6. **The pod's ID and HPKE key in its signing request**, checked by ferund
   against the request, and by `feos-authn` against its own pod; today a
   signing request's names are ignored.
7. **An exec's timeout and output limit** in `ExecRequest`.
8. **The node's key across a reboot under `feos-host-init`**, whose
   `/var/lib/feos` is a tmpfs; until then such a node needs a pinned
   re-join after every boot.
9. **fleet.proto's comment of `IssuePodCredentials`** gains "A request
   repeated with the same key, whose answer was lost, is answered again.
   Too many requests for one pod fail with RESOURCE_EXHAUSTED.", and
   `feos-authn` retries `RESOURCE_EXHAUSTED` with backoff.

## Development

[development.md](../development.md#ferun) has the commands. The tests use
the standard library's `testing`; those that need PostgreSQL make a
database of their own per test in the server `FERUN_TEST_DATABASE_URL`
names, and skip without it. `make go-test` runs them against a throwaway
PostgreSQL 17 in docker. `internal/e2e` runs several instances in one
process against one database, with test nodes.
