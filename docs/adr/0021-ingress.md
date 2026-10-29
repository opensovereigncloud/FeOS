# ADR 0021: The ingress: a program of its own, a peer of the pod mesh, serves environments' hosts over IPv6 with certificates ferund obtains

**Status:** Proposed · **Date:** 2026-10-27

## Context

Every ADR that touched inbound traffic left it open. ADR 0001 names
"load balancers for HTTPS with ACME and for L4, and the way in for IPv4
clients" as the first thing CaaS needs that the sandboxes ruled out.
ADR 0010 says load balancers "are the control plane's front doors and
get an ADR of their own". ADR 0015 keeps them out of `ferund`: "Inbound
traffic to services, CaaS's ingress, is not a front door of `ferund`".
ADR 0017 reserved subnet 0 of a meshed cluster's ULA (ID 0 since its
/96s) and asked about "peers that are not nodes: the control plane's
front doors and load balancers reaching pods' ports over IPv6, at
addresses of subnet 0".
ADR 0018 built the route door "until an ingress does" and lists hosts,
ACME and the way in for IPv4 clients as its open question. This ADR is
that ADR.

What runs today is the route door and the port door of `ferund`. Neither
uses a pod's address: a request goes from `ferund` over the serving
instance's node connection as a `PortForward` stream, through `feosd`,
over vsock, to the guest's loopback. That is the right path for a
sandbox, whose dev server binds the loopback and whose pod no group
opens. It is the wrong path for a service in production: every byte
crosses the control plane and its node connections, which budget 32
forward streams per sandbox and instance and 512 long streams per node
connection beside execs, watches and consoles; the upstream speaks
HTTP/1.1 only, sees `127.0.0.1` as its peer and gets no
`X-Forwarded-*`; a rollout of `ferund` is a rollout of the data path.
In front of the door stands FeCode's backend, one replica replaced with
`Recreate`, which resolves `<label>.boxed.fecode.dev` to an environment
and forwards with its route token: every environment host is down while
FeCode deploys, and a product's workbench is in the data path of the
applications it built. TLS ends at the cluster's ingress controller with
one wildcard certificate. `route.host` is stored and not realised.

The first application to go to production this way is a dashboard a
FeCode project built, at a domain of its owner's, `dashboard.fecore.dev`,
with DNS at Cloudflare that can carry an A record or a CNAME. It has to
stay up while FeCode and `ferund` roll, run in two replicas, and be
reached over IPv4, because that is what its users have.

The network gives the ingress what it needs. Every pod has an IPv6
address of its node's prefix, a /96 of the cluster's ULA on a meshed
cluster (ADR 0010, ADR 0017); a packet reaches it through its node's
`forward` chain, where its security groups allow or drop it by the
source address the mesh vouches for (ADR 0012); its flows are recorded
there (ADR 0013). Only nodes are peers of the mesh today, and the
node's own processes cannot reach pods over IP: the node's `input`
chain takes nothing from a pod but neighbour discovery and DNS, and no
`output` chain filters what the node sends. `ferund` knows every
service's instances, their addresses and readiness, and runs the idle
policy that pauses a service nobody calls; a suspended service is
woken by the door, which holds the request until the pod runs.

## Decision

### The ingress is a program of its own, `ferun-ingress`

- The ingress is FeRun's second program, `ferun-ingress`, built from the
  Go module beside `ferund` (`go/cmd/ferun-ingress`), shipped in the
  same image and as a package beside `ferund`'s. It serves the HTTP
  traffic of environments' hosts and nothing else: no REST API, no
  MCP, no UI, no access to the database and no tenant's token. It runs
  as identical instances, as many as the deployment wants; an instance
  holds no state of its own, only a cache of what `ferund` sent it.
- It is not a front door of `ferund`, as ADR 0015 decided: `ferund`'s
  instances carry no application traffic, a rollout of `ferund` cuts no
  request, and an outage of `ferund` stops no traffic, because the
  ingress serves from what it has. It is not a role of the node either:
  nodes open no port but the mesh's (ADR 0001), and the ingress runs
  where public addresses are.
- An instance connects out to `ferund`, as a node does (ADR 0015): it
  joins with a one-time token an operator issues, holds a certificate of
  the platform CA with the ID `spiffe://<trust domain>/ingress/<name>`,
  renews it over the same connection, and takes everything it serves
  from `ferund` on that connection. `ferund` opens nothing to it.

### A peer of the mesh at subnet 0

- Every instance has a WireGuard key pair of its own, made at its first
  start and kept in its state directory, and an endpoint at which the
  nodes reach it. `ferund` gives it an address of subnet 0 of the
  cluster's ULA, `<ULA /48>:0::<n>/128`, from a counter that never gives
  a number out twice; subnet 0 as a whole, `<ULA /48>:0::/64`, is the
  **ingress block**. ADR 0017's question is answered so: the first peers
  that are not nodes are the ingress's instances.
- `ferund` sends every node the instances as peers, each with its /128
  as the one prefix allowed for it, in the same table as the nodes and
  with the same versions and deltas; and it sends every instance the
  nodes as its peers, with their /96s. A node speaks for its /96, an
  instance for its /128, and nothing else comes through the mesh
  (ADR 0017). A packet from the ingress reaches a pod through the pod's
  node's `forward` chain like a packet from another node's pod: the
  security groups decide, the reverse path rule holds, and the flow is
  recorded (ADR 0013). The node changes nothing but what a peer may be.
- Pods are denied the ingress block as a destination, as they are denied
  the nodes' addresses: an instance listens on nothing at its mesh
  address. On a cluster whose nodes have routed prefixes instead of a
  mesh (ADR 0010), the ingress block is a prefix the operator routes to
  the instances and names to `ferund`; the groups do not care which.
- On Kubernetes an instance is an ordinary pod with `NET_ADMIN` and a
  network namespace of its own, so that its WireGuard device and its
  route for the cluster's /64 never meet a node's on one host; its
  endpoint is its pod address, which the nodes' hosts reach, and which
  `ferund` sends anew when the pod is replaced. On one machine, where
  the ingress runs beside `feosd` (ADR 0015), it runs in a network
  namespace of its own for the same reason, a peer of its own node over
  the loopback.

### A route opens one port of one service, and nothing else

- For every service a route names, `ferund` keeps a security group
  `in-<environment>-<service>` whose members are that service's
  instances and whose one rule allows TCP on the service's routed ports
  from the ingress block. A service no route names, an addon, a job and
  a sandbox stay closed to the ingress, whatever the ingress's table
  says; the group is the boundary, the table the routing.
- An image service binds `[::]`, as ADR 0010 says; a program that binds
  `127.0.0.1` or `0.0.0.0` alone cannot be reached over IPv6, and its
  readiness check says so before the route does. A service of runtime
  `attached` runs in a sandbox, on its loopback, and no address of the
  mesh reaches it: routes to attached services are served by the route
  door as before, and never by the ingress (an info finding of plan
  says which door a route is on). FeCode's previews and its preview
  environments, whose services are attached, stay behind the doors and
  behind FeCode's forwarder, where their authorization belongs (FeCode's
  ADR 0001 §4); stage, production and demo, whose services run images,
  move to the ingress.

### Hosts: a default host per environment, domains per tenant

- The platform has an **apps zone** (`--apps-zone`, such as
  `apps.fecode.dev`), whose wildcard DNS points at the ingress. A tenant
  may be given a zone of its own instead. Every environment has a
  default host, `<environment>.<zone>`, from its creation; the
  environment's name is unique in its tenant, and the tenant's zone is
  the tenant's. A route whose `host` is empty answers on every host of
  its environment.
- A **domain** is an object of the tenant, bound to one environment:
  `dashboard.fecore.dev` is a domain of the tenant `fecode` bound to the
  dashboard's production. It is claimed by a call, moves to another
  environment of the tenant by a call, and both are recorded; no
  release moves a domain, since a release is one environment's and a
  domain's move concerns two. A route's `host` is realised: a route with
  a host answers on that host, which must be a domain of its environment
  or its default host, else the write fails with a finding.
- Before a domain is served, `ferund` verifies that whoever claimed it
  controls it: the name resolves to the ingress, by a CNAME to the
  platform's edge name (`edge.<domain>`, which the operator publishes
  with the balancer's addresses) or by A and AAAA records with those
  addresses; and, where another tenant holds the name or claimed it
  before, by a TXT record `_ferun-challenge.<name>` that carries a token
  of the claim. A name in the apps zone or a tenant's zone cannot be
  claimed. `ferund` checks again at an interval and marks a domain whose
  DNS went elsewhere; the environment gets an incident, the domain is
  served on until its certificate expires.

### Certificates: ferund is the ACME client

- `ferund` obtains and renews every certificate the ingress serves, with
  one ACME account per platform (`golang.org/x/crypto/acme`, a package
  of a module `ferund` depends on already). A domain's certificate comes
  over HTTP-01: `ferund` keeps the challenge in the ingress's table, and
  every instance answers `/.well-known/acme-challenge/<token>` for that
  host on port 80, so that a CNAME is all a tenant's DNS needs. A zone's
  wildcard certificate comes over DNS-01 where `ferund` holds the zone's
  DNS credential (Cloudflare first, the provider a small interface), or
  from the operator, who puts a certificate and its key in through the
  admin API and renews it as they renew it. Renewal starts at two thirds
  of a certificate's lifetime; a renewal that fails for a day is an
  incident on the domain's environment.
- Private keys are made by `ferund`, sealed in the database under the
  key that seals secrets (ADR 0018), and sent to the instances over
  their connections. An instance keeps them in memory and in its cache
  (below), readable by its own user alone. An ingress machine is as
  trusted as a node, which holds every pod's memory.

### The table: what an instance serves from

- `ferund` sends every instance one table, whole at connect and then as
  deltas with versions, as it sends groups and name tables to nodes
  (ADR 0012, ADR 0018): for every host its environment and certificate;
  for every environment its routes (path, service, port, `strip`, the
  port's protocol) and its state; for every routed service its instances
  with their addresses and readiness, and whether the service idles; the
  ACME challenges; the platform's default certificate for hosts it does
  not know. An instance serves from the table and from nothing else: a
  host that is not in it gets TLS's `unrecognized_name`, and `404` on
  plain HTTP.
- An instance keeps the last table and its certificates in its state
  directory and serves them through an outage of `ferund` and a restart
  of its own, as the nodes keep their peers and groups: what was
  routable stays routable while the control plane is away, and nothing
  new becomes so. A table older than an hour is served with a warning in
  the instance's status; nothing expires it.

### Requests

- An instance terminates TLS with the host's certificate by SNI, takes
  HTTP/1.1 and HTTP/2 from clients, and speaks HTTP/1.1 to the service,
  or h2c where the port's protocol says so (gRPC). Upgrades and
  WebSockets pass; answers stream unbuffered. It sets `X-Forwarded-For`,
  `X-Forwarded-Proto` and `X-Forwarded-Host` from the connection and
  drops a client's own, and keeps the `Host`; an application behind the
  ingress sees who called it, which no application behind the door does.
  Plain HTTP on port 80 answers ACME challenges and redirects everything
  else to HTTPS.
- A request goes to a ready instance of the route's service, round robin
  per ingress instance, over kept connections; a connection that fails
  before a byte of the answer is tried once more on another ready
  instance when the method is idempotent. An environment whose latest
  release has failed is served by the instances of the realised one, as
  the reconciler keeps them (ADR 0018); the table says which are ready.
- **Waking.** When a routed service has no ready instance because its
  pods are suspended, the instance asks `ferund` on its connection to
  wake the service and holds the request until the table shows a ready
  instance, for the resume's budget and at most two minutes, then
  answers `503` with `Retry-After`. The wake is `ferund`'s: it resumes
  or restores a pod as the door does today (`Ensure`), and the policy
  that suspended it stays where it is.
- **Activity.** Every ten seconds an instance tells `ferund` which
  services had a request or an open connection; `ferund` counts that as
  the service's activity, in place of the door's touch and hold, so that
  the idle policy pauses a service nobody calls and never one with a
  connection open. Pod-to-pod traffic counts as before: not at all.
- **Records.** For every request the instance keeps a record: host,
  method, path, status, bytes in and out, duration, the client's
  address, and the environment and service it went to. It sends them to
  `ferund` in batches, bounded; what overflows is dropped and counted,
  as the node's traffic records are (ADR 0013). `ferund` stores them as
  traffic records of a kind of their own, `http`, attributed to the
  tenant and the environment, under the same retention. The flows
  themselves are recorded by the pod's node as any flow.
- The ingress authorizes nothing: what a route of a public environment
  names is public. An environment is public by its tenant's decision;
  the route door, which takes the environment's route tokens, serves
  every environment. Timeouts and a body limit have platform defaults;
  rate limits and settings per domain are open.

### What stands in front

- On Kubernetes, a Service of type `LoadBalancer` for ports 80 and 443
  to the ingress's pods, with `externalTrafficPolicy: Local` so that the
  client's address arrives, gives the platform a public address beside
  the cluster's ingress controller, which goes on serving the platform's
  own hosts (FeCode, `run`, `id`, `oci`). The operator publishes the
  address as `edge.<domain>`, and the apps zone's wildcard, and domains
  CNAME to it. The cluster's ingress controller is not in the path: it
  would own the certificates, or pass TLS through by SNI and add a hop
  and a second place where routes are configured. On bare metal, the
  instances' own addresses stand in DNS, or an L4 balancer passes TCP
  to them.
- Clients reach the ingress over IPv4, and over IPv6 where the balancer
  has it; pods' addresses are never public. The ingress is the way in
  for IPv4 clients that ADR 0001 asked for.

### What changed while it was built

The core was built without domains, ACME and `http` records. Building it
changed these points:

- **The route door is not open, so the ingress serves only public
  environments.** The draft said the door authorizes nothing. It does:
  it takes only requests with the environment's route token, which its
  tenant mints. The ingress authorizes nothing. It therefore serves an
  environment only after its tenant makes it public (`PUT
  /api/v1/environments/{env}/public`). An empty apps zone switches every
  host off.
- **A prefix per instance from the nodes' counter, not subnet 0.** Every
  instance takes the next ID of `mesh_subnets` and uses `<prefix>::1`:
  a /96 since ADR 0017 gives nodes /96s, a /64 before. Nodes take it as
  an ordinary peer `ingress.<name>` of a node's length, so `feosd`
  changes nothing. ID 0 stays reserved. A re-join keeps the prefix.
- **Groups: `ing-<sandbox id>` per pod and `ferun-ingress`, not
  `in-<environment>-<service>`.** The proposed ID is longer than
  `feosd`'s 64 characters, and a pod names its groups at its creation.
  - The pods of an environment's image services name their own `ing-`
    group once the environment has been public. Its first publish rolls
    them once.
  - While the environment is public, the group's one rule allows TCP on
    the ports its service's routes use (in the realised and the latest
    release) from `ferun-ingress`.
  - `ferun-ingress`'s members are the instances' addresses. Its version
    is a counter bumped under a row lock, so versions follow commit
    order.
  - Route changes and publishing change rules, not pods.
- **Admission.** An instance sends to an endpoint only when three things
  hold:
  - its node holds the `ing-` group with that port;
  - its node holds `ferun-ingress` at a version that contains the
    instance;
  - the instance has a WireGuard handshake with that node younger than
    180 s.

  The last condition catches a node that never took the instance as a
  peer, and blocked UDP.
- **Identity.**
  - Certificates are renewed at half life. A newer certificate presented
    supersedes the older ones after 10 minutes.
  - Only the instance's newest certificate is renewed, for its own key,
    and only near its renew time; a superseded one is not renewed even
    within its 10 minutes. A copied certificate therefore dies with its
    lifetime once the instance has renewed, instead of renewing itself
    beside the instance for ever.
  - The instance's WireGuard key is fixed from its first Hello after a
    join, so a stolen certificate cannot take its place on the mesh.
  - Every join brings a new WireGuard key: the agent makes one after each
    join, and a join again refuses the key before it. The documented
    recovery from a stolen state directory, a removal and a join again,
    thus also takes the thief's copy of the key off the mesh.
  - A WireGuard key is one peer's, across nodes and ingress instances,
    checked under one row lock; keys of small order are refused. The
    nodes' devices keep one peer per key, and two peers with one key
    would take each other's prefix.
  - An ingress's join token has a form of its own,
    `feos-ingress-join-<secret>.<pin>`, which a node's Join refuses
    before any lookup, also in a `ferund` that knows no kinds: the kind
    column alone left an older instance free to join it as a node.
  - Each pod sees only its own join token: an init container copies its
    key of the Secret into a memory volume, and the instance, which faces
    the internet, does not mount the Secret, so it cannot take the other
    instance's identity over with that one's token.
  - An instance gives up its identity only when it was removed or
    refused for minutes, and joins again only with a token it has not
    tried.
- **The connection.**
  - gRPC on the nodes' port with an ALPN of its own, `ferun-ingress/1`.
  - JSON-coded messages defined in Go (`go/internal/ingress/wire`),
    because protoc is not part of the toolchain.
  - One connection to one instance of `ferund` at a time: from a list of
    addresses, a failover is a reconnect and a whole table.
- **The table** is computed by every `ferund` instance with ingress
  streams, from the database:
  - triggered by NOTIFY `ferun_ingress` and `ferun_sandbox`, with a 5 s
    backstop;
  - it reaches the ingress about 100 ms after the write;
  - it distinguishes asleep, waking and pending instances, and a request
    is held up to 2 minutes while instances are asleep or waking, 10 s
    while they are pending;
  - it is bounded per environment, not in total, so that no tenant can
    make it too large for every instance: an environment whose entry
    would take more than 2 MiB is left out, and a route's path has at
    most 1 KiB. A table larger than 4 MiB goes in parts, which an
    instance applies together once the last one has arrived.
- **Activity is a hold, not a touch.**
  - For a service with an idle policy, the instance holds the pod as the
    port door does (kind forward, holder `ingress:<name>`), admitted
    under the row lock before the first request, so an idle pause never
    freezes a request.
  - While an instance has lost `ferund`, idle pauses of public
    environments' services are fenced off for up to 15 minutes.
- **Hosts.** Each public environment has one default host label under
  the apps zone:
  - `<environment>-<tenant>`, or the name's start and a hash, given at
    its first publish;
  - registered to its tenant, so it is never handed to another one;
  - served with one wildcard certificate that the operator provides as
    files.

  A host the table does not know gets 404.
- **A natural label names its tenant.** `<environment>-<tenant>` with
  the tenant's dots as hyphens read the same for different pairs:
  `shop` of `acme.io`, `shop` of `acme-io` and `shop-acme` of `io`.
  Whoever published first kept the label for good, so another tenant
  could hold the host that reads as yours. The natural label is given
  only when it names one pair: the environment's name has no hyphen, the
  tenant no dot, and the label does not end as a fallback does (a hyphen
  and 16 hex characters). Every other environment gets the fallback.
  Refusing tenant names that collide after the mapping would not cover
  the hyphen between name and tenant, and a discriminator on every label
  would make simple names unreadable.
- **The apps zone is `apps.fecode.dev`, beside the platform's hosts, and
  the ingress makes apps' cookies host-only.** Default hosts live under
  the platform's domain (the Gardener deployment derives `apps.<domain>`
  from its domain); tenants' domains come later, beside it. An app under
  such a zone could set cookies for `fecode.dev`, which reach `run`, `id`
  and FeCode (cookie tossing, login CSRF), or for the other apps. So on a
  host under the apps zone the ingress removes from every `Set-Cookie` of
  an answer each `Domain` attribute that names anything but the request's
  host: the cookie becomes host-only and reaches neither the other apps
  nor the platform's hosts. A proxy cannot stop cookies an app's own
  JavaScript sets (`document.cookie`): the platform's hosts guard against
  those with `__Host-` session cookies, as FeCode's are, and a Public
  Suffix List entry for `apps.fecode.dev`, an operator's action, is
  recommended.
- **Retries and limits.**
  - A connection that cannot be dialled is tried once on another
    instance for every method. One that fails after the request was
    written is retried only for GET, HEAD and OPTIONS.
  - Pooled upstream connections idle at most 4 s.
  - A body limit and a connection cap per endpoint have defaults.
    Upstreams speak HTTP/1.1 only. Port 80 redirects.
- **One tenant cannot take an instance from the others.** An app that
  never answers, reached by many HTTP/2 streams, would otherwise pin
  memory until the shared instance is killed, and a service that stays
  pending would fill the held requests and turn every other cold start
  into a 503.
  - Forwarded requests, from the pick of an endpoint to the end of the
    answer or of the upgraded connection, have a share per tenant, a
    share per environment within it, and a cap per instance that fits
    its memory limit; held requests have the same two shares beside their
    cap. Beyond, the answer is 503. A tenant's share is a quarter of the
    cap, so that more public environments do not let one tenant fill it.
  - Each wait for a connection to an instance is bounded; the answer's
    time is not, for long polls.
  - An HTTP/2 connection carries at most 63 streams, and the Go
    runtime's soft memory limit sits at 90% of the container's.
  - What a waiting request keeps is bounded too, so that the caps bound
    memory: an HTTP/2 client may send 64 KiB of a body before it is read
    (HTTP/2's own window; Go's default of 1 MiB let one tenant's held
    requests pin 280 MiB), headers take at most 32 KiB in 100 fields
    (Go's default of 1 MiB let one request take several MiB), and a
    request that announces trailers is refused. The default caps then
    take at most about 950 MiB, and an instance's limit is 1.5 GiB.
  - A cap per client address waits for the PROXY protocol.
- **A change of the table cuts the connections to what is no longer
  routed.** The nodes keep established flows after their rules shrink,
  so the ingress closes the open connections itself, upgraded ones
  included: at once when the environment, its service or the instance's
  port is no longer routed, so that unpublishing ends every way in; 30 s
  later when only the instance left a service that stays routed, so that
  a retiring replica's requests end with its pod.
- **On Kubernetes** the ingress is a StatefulSet:
  - a volume per instance; root with `NET_ADMIN` alone, under the
    runtime's default seccomp profile, not Unconfined;
  - a privileged init container, as root, that enables IPv6;
  - a NetworkPolicy that keeps the health port and WireGuard to the
    nodes, and one that lets the instances dial only DNS, `ferund`'s
    nodes' port and the nodes' WireGuard, so that an instance taken
    over through its proxy reaches nothing else in the cluster;
  - the instances on different workers, by topology spread;
  - a balancer with `externalTrafficPolicy: Cluster`, not `Local`: the
    IronCore balancer checks no node's health, and with `Local` a worker
    without an instance drops its share of connections all the time, and
    one whose instance restarts in every rollout. Applications therefore
    see a worker's address in `X-Forwarded-For` until the PROXY protocol
    (open questions).
- **A service must listen on `[::]`.** Its readiness check, through the
  port door on the loopback, does not show it; the ingress answers 502
  with that hint.

## Alternatives considered

- **The route door as the ingress**, `ferund`'s instances the data
  plane, with hosts and certificates added to them. Every byte through
  the control plane's node connections and their budgets, HTTP/1.1
  alone, no client address, and a rollout of the control plane in every
  request's path. ADR 0015 kept the ingress out of `ferund` for this.
- **The ingress on every node**, a program beside `feosd` that uses the
  node's mesh device and reaches its own pods over their TAPs. No new
  kind of peer, but every node a public listener, an `output` chain and
  `input` rules for the node's own packets, every domain's key on every
  node, and an edge tied to the node's drains and rollouts. A fleet of
  many nodes wants an edge pool of its own; an edge pool of its own is
  what this ADR builds, and it may run on nodes where that is all there
  is.
- **The node's vsock door from a sidecar** (`/run/feos/api.sock`): the
  same-node pods alone, no groups and no records, and vsock's limits.
- **Kubernetes's ingress**, the cluster's controller with cert-manager
  and an `Ingress` object per domain that `ferund` writes. The routes
  and certificates of the platform as Kubernetes objects, a second
  owner of the edge, and a platform that runs without Kubernetes
  (ADR 0001, ADR 0017) tied to it on the one path every customer uses.
- **A provider's edge** (a tunnel to a CDN) in front of the doors: a
  dependency on one provider for every tenant's traffic; a tenant may
  put one in front of its domain by itself.
- **WireGuard in userspace** in the ingress process, without
  `NET_ADMIN`: every packet through userspace and a dependency of its
  own; the kernel's device under a capability is what the nodes do.
- **Inbound IPv4 to pods**, a public IPv4 per service: ADR 0010 made
  IPv4 the family for leaving; the ingress is the way in.
- **Waking a pod by a packet** (ADR 0010's question): the ingress holds
  the request and knows what it waits for; a wake on a packet would
  need the node to hold connections for a pod that is not there.
- **Certificates in cert-manager**, read by `ferund` from the cluster:
  one more thing that works only on Kubernetes, for an ACME client of a
  few hundred lines.
- **Authorization at the ingress for previews**, so that FeCode leaves
  the data path altogether: FeCode's ADR 0001 keeps it in FeCode's
  forwarder, which knows the person; previews are a workbench's, not a
  customer's. An authorization hook for a tenant stays open.

## Consequences

- FeRun gains a second program and a second kind of peer. `feosd`'s
  mesh peers may be an ingress instance with a /128; nothing else of the
  node changes. `ferund` gains the ingress's join, identity and
  connection, the ingress block and its counter, the groups
  `in-<environment>-<service>`, the table and its deltas, the wake call,
  activity from the ingress, traffic records of kind `http`, domains
  with their verification, zones, an ACME client with HTTP-01 and DNS-01,
  sealed keys, and the admin API for all of it. `golang.org/x/crypto`'s
  `acme` packages are added to the module's dependencies.
- The route door stays, for sandboxes and attached services, with its
  route tokens; ADR 0018 is amended so: routes to image services are
  realised by the ingress, routes to attached services by the door.
  ADR 0010's sentence that the control plane's front door reaches pods'
  ports over IPv6 becomes true of the ingress; the doors keep vsock.
- FeCode's forwarder stops serving stage, production and demo, which
  get the apps zone and their domains; its previews and preview
  environments stay as they are. How FeCode shows domains, builds and
  the ingress's records is FeCode's ADR 0009.
- An application behind the ingress sees the client's address and
  protocol, may speak HTTP/2 and gRPC to its callers, and is reached
  while FeCode deploys and `ferund` rolls.
- The reference deployment runs three instances of `ferund` (ADR 0015)
  with anti-affinity and a disruption budget over a PostgreSQL that is
  replicated synchronously, as `docs/architecture/control-plane.md`
  requires, in place of today's one instance over one PostgreSQL; two
  instances of the ingress; and a second `LoadBalancer`. Nothing of
  `ferund`'s code stands in the way: nodes connect to every instance
  and any instance serves any request already.
- Operators publish `edge.<domain>` and the apps zone's wildcard, open
  port 80 to the ingress for HTTP-01, and hand `ferund` a DNS credential
  for the zone's wildcard or a certificate.
- The known limits of this version: HTTP alone, no L4; no authorization
  and no rate limits at the ingress; the ingress block as one prefix in
  every routed service's group; records in PostgreSQL.
- Built in this order: the program with its join, mesh peer, table,
  groups and default hosts under the apps zone with an operator's
  wildcard, behind the balancer, so that a stage environment is reached
  with nothing of FeCode or `ferund` in the path; then domains with
  verification and HTTP-01, so that `dashboard.fecore.dev` is served;
  then waking, activity and records; three instances of `ferund` and
  the replicated PostgreSQL go first, since they need no code.

## Open questions

- **L4 routes:** TCP and UDP services, and TLS passed through by SNI to
  a service that terminates it itself.
- **Authorization at the ingress** for a tenant: a signed cookie or a
  forward-auth call to the tenant's service, so that FeCode's previews
  could leave its forwarder, and a stage could be closed to the public.
- **Rate limits, body limits and timeouts per domain**, and what shields
  the ACME account from a tenant who claims domains by the hundred.
- **Certificates a tenant brings**, for a domain whose CA is not
  Let's Encrypt, and DNS-01 providers beyond the first.
- **A Public Suffix List entry for the apps zone**, so that one
  environment's page cannot set a cookie for the zone (FeCode's
  ADR 0008, question 8): the ingress makes the cookies of answers
  host-only, not those a page's script sets; domains sidestep it for
  production.
- **The balancer's client address** where `externalTrafficPolicy: Local`
  is not available: the PROXY protocol.
- **The proxy apart from the mesh's privileges.** One process parses
  the internet's requests and holds `NET_ADMIN` as root, confined only
  by the seccomp profile and the egress policy. The shape that removes
  that: the agent, root with `NET_ADMIN`, and the proxy, nonroot
  without capabilities, as two containers in the pod's one network
  namespace, the proxy handed the table, the handshakes and its wakes
  and holds over a local socket; or, narrower, only the mesh device's
  three calls behind such a socket in a small privileged container
  (`docs/architecture/ingress.md`, Privileges).
- **Where the ingress runs at scale**: a pool of its own, anycast
  addresses, and the records' home when they outgrow PostgreSQL
  (ADR 0015's question on traffic records).
- **An environment's default host off**, for a production that wants to
  answer on its domain alone.
