# ADR 0018: Environments and releases: FeRun's CaaS API takes typed objects, and every change is a numbered release

**Status:** Accepted · **Date:** 2026-10-18

## Context

ADR 0001 makes CaaS a product with an API of its own, modelled on what
Kubernetes does for a pod and not on Kubernetes, and puts "services with
replicas and routes" in the control plane. ADR 0004 gives a pod what a
service needs: container roles, a restart policy, probes, jobs that end.
ADR 0016 gives it persistent volumes, ADR 0012 security groups, ADR 0015
the control plane, `ferund`, in replicas over PostgreSQL. Nothing of CaaS
is built in `ferund` yet: its registry holds sandboxes alone, and the
sandboxes have settled the shape of everything in it: a row is the truth,
every change an op under the row's lock, a spec pinned by digest and the
same on every retry, idempotent by construction.

Two callers will operate CaaS. The first is an agent in a FeCode sandbox,
writing an application and needing to say how it runs: which services,
which databases, which routes, and to learn at once whether what it said
holds. The second is a person: deploying a version, reading what changed
in production last month, copying an environment for a new region or a
customer's demo. A third, a CaaS customer without FeCode, drives the same
thing from a CLI or a CI.

What happens without the platform is on record. An agent built a demo
application in FeCode, Expensa: five services, Postgres, NATS. It wrote an
ADR about running it, a compose file, a Caddyfile, an init SQL for roles
and schemas, and, because the sandbox has no Docker, a shell supervisor of
three hundred lines that installs Postgres from apt, builds `nats-server`,
starts the services with PID files, waits for their health, keeps logs and
proxies ports in place of Caddy. The compose file never ran; the agent
noted that it was checked by reading. Every line of that work is the
platform's work, done again per project, never the same twice and never
what production would run. An agent routes around a missing platform by
building one; the platform has to be where the agent is, with the service
graph as objects it can state and get an answer for at once.

The alternative in view was GitOps: the declaration as a file in the
repository and a controller that pulls it. The file would need a format
(a compose file is what a model writes unprompted, and says nothing about
managed databases, routes, environments or secrets), a validator in the
agent's loop, and a second store for the secrets and the pointer of each
environment regardless; and its history would be the history of one file,
not of an environment.

## Decision

### An environment holds typed objects

- An **environment** is the unit of CaaS in `ferund`: a tenant's, with a
  name, a security group of its own (ADR 0012) so that its pods reach each
  other and nothing else unless a rule says so, and a name space in which
  its services address each other by name.
- It holds objects of six kinds, each with a name unique in the
  environment:
  - A **service**: a pod template per ADR 0004 (its image pinned to a
    digest, its command, its environment, resources, a readiness check),
    a number of replicas, the ports it serves, and its idle policy: after
    how long without a request its pods are suspended, as sandboxes are
    paused. A suspended service's pod is resumed on the first request,
    which its route holds until the pod runs (ADR 0004).
  - A **job**: a pod that runs to its end. A job `on: release` runs before
    the release's services move (a migration); any job runs when asked.
    How often a job may fail and how long it may run is the
    environment's to say.
  - An **addon**: a managed backing service by type and version,
    `postgres`, `valkey`, `nats`; a plan, which is its size; and the
    options of its type (databases and roles for Postgres, JetStream for
    NATS). `ferund` realises an addon from a blueprint per type: its pod,
    its volume (ADR 0016), credentials it generates, health and
    provisioning. Its outputs, addresses and credentials, are what
    services reference. An addon is one instance in its first version;
    replication is a decision per type. Backups to the object store,
    minor upgrades and more types (`clickhouse`) come later.
  - A **route**: a path, a service and a port, and a host, stored and
    not yet realised. `ferund` realises routes for HTTP itself, in a
    route door of its front door (below), until an ingress does.
  - A **volume**: a persistent volume (ADR 0016) that a service's pods
    mount. A volume attaches to one pod, so a service with one has one
    replica. Not built yet: a spec with volumes or mounts is refused with
    a finding; addons have volumes of their own.
  - A **secret**: a name and versioned values, stored sealed under the key
    the bindings rest at. A release pins the version; the value reaches a
    pod as an environment variable, and the API never returns it.
- Objects reference each other by name, and the references are resolved
  when a write is validated: a service's environment variable from an
  addon's output (`{addon: postgres, role: expense_svc, key: url}`), from
  a secret, or from another service (`{service: approval, port: http,
  key: url}`), a route's service and port, a job's image. A dangling
  reference is an error of the write, not a pod that fails at start. An
  addon's output resolves to the address of its instance, so that a
  dependent's pod changes, and is rolled, when that address changes; a
  service's resolves to its name, which a hosts block resolves (below).

### A service may run inside a sandbox

- A service's **runtime** is `image`, pods of its own as above, or
  `attached`: it runs inside a named sandbox of the same tenant, as a
  supervised process in the sandbox's main container, with a command and
  a working directory in the sandbox's tree, under readiness checks,
  restarts and logs, with the environment's addons and image services
  reachable by name. This is how FeCode's agent runs the application it
  is writing: from source, in its own sandbox, while Postgres and NATS
  are real. Stage and production run `image`. A process needs nothing of
  the node: `ferund` starts, watches and stops it through the agent's
  process tools, and resolves its environment, secrets included, into
  the start's request, never onto the sandbox's disk.
- A sandbox is **attachable** when it is created so: it gets a security
  group of its own, `sbx-<id>`, with egress to the environments whose
  services it runs and no ingress, so that it reaches their addons and
  image services and image pods never reach it. A sandbox created before
  cannot reach an addon.
- An attached service uses its sandbox while that sandbox exists, is the
  environment owner's and is no environment's pod. The owner's sandbox
  deleted and created again under the same ID, as FeCode replaces a lost
  workspace sandbox, is a new incarnation, in which the service starts
  anew, its restarts and checks counted from zero; a sandbox ID that
  another tenant took is never the service's, and the service fails
  rather than start, route or reach there.
- A service that goes, removed, moved to another sandbox or with its
  environment deleted, has its process stopped and its names taken out
  of the sandbox's hosts file first, a sleeping sandbox woken for it; its
  record goes or moves only then, or once the process is gone with its
  sandbox for certain, so that no process runs on with the environment's
  secrets.

### Every change is a release

- The objects of an environment are not changed in place. A write, an
  `apply` of the whole desired state or an operation on one object (put a
  service, remove a route) that `ferund` completes to the whole, produces
  a **release**: a number, monotonic per environment; the complete desired
  state; who, when, and a message; the release it was based on; and
  annotations, a small string map the caller keeps with it (FeCode's
  definition revision, the hash of its values, what kind of change it
  was). Releases are immutable and kept.
- **Plan** takes the same input as a write and answers with what the write
  would do: its validation findings, each with the path of the object and
  field it concerns and what would make it pass, and the diff against the
  current release, object by object, with what each change does to the
  running pods. Plan writes nothing. Apply runs the same validation and,
  when it passes, records the release. The findings are worded for
  whoever writes the objects, a model included: the path, the problem, the
  remedy.
- Apply pins every image to its digest and every referenced secret to its
  latest version, and stores the release so: a release names what it
  runs, and a revert runs the same bytes. Plan only warns about an image
  that does not resolve; apply refuses it.
- A write names the release it was planned against, its `base`. When the
  environment has moved since, the write fails with a conflict that names
  the current release, and the caller plans again: the compare-and-swap of
  ADR 0012's `from_version`. A write that changes nothing answers with the
  current release, whatever its base, unless that release failed: then
  the same spec makes a new release, which is how a failed one is tried
  again.
- **Revert** to release K is a new release with K's content and K's
  annotations, marked as K's revert. A **tag** names a release (`v1.4`,
  `demo-2026-10`) and moves only by an explicit call, which is recorded.
  The history of an environment is the list of its releases with their
  diffs: what changed in production in October is a query.
- A secret's new value is a new version and a release that pins it, so
  that a rotation is in the history as a rotation, never as a value.

### The reconciler works toward the latest release

- An environment's pods are sandboxes: each instance, a service's
  replica, an addon, a job's run, is a row of the sandboxes' registry
  that names its environment and carries a pod template, so that the
  sandboxes' engine places, starts, moves, suspends, hibernates and
  deletes it with its ops, locks, tombstones and holds, unchanged. The
  environment's controller inserts and deletes those rows and keeps its
  own view of them. The pods' labels carry environment, object and
  template, not the release: an object a release does not change keeps
  its pods.
- A release's rollout runs its `on: release` jobs first, after the
  addons are provisioned, then moves each service replica by replica, a
  new pod ready before an old one goes, routes following readiness. The
  release is **realised** when every object matches it; the status names
  the release realised and the one in progress.
- A rollout that does not reach readiness within its budget has
  **failed**: the service keeps the pods of the last realised release
  serving, the new ones are removed, and the environment stays on its
  failed desired release until the next one, a revert or a fix. Each
  service has a budget (`rollout.timeout`, 10 minutes by default), and
  the release one beyond them: the longest of its services' budgets, its
  release jobs' time with their retries, and 10 minutes, after which the
  release fails naming what it still waits for, so that a release that
  waits for an addon or for capacity never rolls forever. A job runs at
  most once at a time in its environment, across releases: a release
  waits for a run of the job that an older release or a person started,
  never cancels it, and takes its success under the same template as
  its own; the time it waited moves the start that its deadline and its
  services' budgets count from. While its latest
  release has failed, the controller keeps the realised one running and
  healed, and never tries the failed one again. `ferund` writes no
  release of its own. Whether a failed rollout is reverted at once is a
  policy of the caller, FeCode's per environment class, not a mechanism
  that invents history.
- An addon's volume holds data of one type, written by a version: the
  volume decides what may run on it. An addon whose type changes is
  removed and added, its data deleted, as plan's warning said; a version
  the blueprint cannot run on the data (an older major version of
  Postgres) is refused, the running instance kept. While a release that
  recreated an addon has failed, the addon stays as that release has it,
  never recreated back onto older software. An addon that only a failed
  first release has keeps its data.
- An addon whose pod a node that is up has lost is replaced on that
  node, on its volume, when the node reports the volume free; one lost
  with a node that is down or removed, or whose volume is missing there
  or attached to another pod, is not replaced, its data being where it
  cannot run, and its incident says what the node answered.
- Until the guest supervises containers as ADR 0004 decides, `ferund`
  runs the readiness checks over the node's connection and restarts a
  service's or addon's exited container by restarting its pod, with a
  backoff up to 5 minutes; 3 exits within 10 minutes are a crash loop.
- What the reconciler cannot resolve ends in an **incident** on the
  environment: the object, the condition (a failed rollout, a crash loop,
  a failed job, no capacity, an addon that cannot run on its data, a
  volume that cannot attach), and the evidence `ferund` has: the pod's
  last exits and restarts, its log tail, redacted of the environment's
  secrets and bounded in size, the placement's verdict, what the release
  waited for, the release diff since the last realised one.
  An incident is opened once per object and release and closed when the
  condition clears. Incidents are the hook for whoever operates: a
  person, or FeCode's agent.

### The environment's network

- An environment's names are its addons and image services. `ferund`
  writes them, with their addresses, into a block of `/etc/hosts` in each
  of the environment's service and job pods and in each sandbox its
  attached services run in (there with the sandbox's own attached
  services as loopback), and rewrites the block when an address changes
  or a pod it names retires. A service and an addon therefore do not
  share a name, and two environments whose attached services share a
  sandbox share neither a name of its hosts file nor a port there. An
  attached service reaches by name only the attached services of its
  own sandbox.
- Its security group, `env-<id>`, lets its members, the environment's
  pods and the sandboxes of its attached services, reach each other, and
  every pod the internet: an egress table of the global unicast blocks of
  both families without the special-purpose ranges (feosd's internet
  sets), never `::/0` or a bare `0.0.0.0/0`. `ferund` puts the groups on
  a node before the pod that names them, whole.

### Routes reach services through `ferund`

- `ferund` serves an environment's HTTP routes in a door of its own,
  `/api/v1/environments/{env}/http/…`, as it serves previews in the port
  door: the longest route prefix of the realised release picks the
  service, a ready instance takes the request (a suspended one is woken),
  and the request goes on through the port door's machinery. The door
  takes only a **route token** of that environment, minted by its tenant
  and shown once, so that public traffic never travels with a tenant's
  credential. The token travels in a header of its own,
  `X-FeRun-Route-Token`, which stops at the door, so that the
  application gets the client's `Authorization` as it was sent; a client
  that sends the token as its bearer token gets no `Authorization`
  through. FeCode serves an environment on a host of its own and
  forwards to the door with the environment's token in that header.

### An environment is copied from a release

- `clone` makes a new environment from a release of another: the
  source release's objects, or a whole spec of the caller's, which is
  how a copy gets other plan sizes or replicas. Secrets are copied only
  where each is named. Attached services are not copied: they run in a
  sandbox of the source. An addon's data comes along only when asked,
  from the addon's backup, and a volume's from its checkpoint (ADR 0016),
  both later; a clone's addons start empty. A copy for a new region and a
  demo for a customer are a call and a plan; idle, the copy costs what
  suspended pods cost.

### One API for every caller

- FeCode's backend for its projects' environments, an agent through
  FeRun's MCP tools (plan, apply, status, logs and incidents on the
  environments its token may see), a person through a CLI or FeCode's UI,
  a customer's CI: all use the same API. It accepts objects. A bundle, the
  whole desired state as one JSON document, is one input of `apply` and
  the export of a release. GitOps is then a way a caller chooses to drive
  the API, not the architecture. The REST API is built; the MCP tools and
  their grants, the CLI and an admin's listing of every tenant's
  environments come later.

### What changed while it was built

The decision was taken as proposed above; building it changed these:

- **Instances are sandbox rows**, the engine reused as it is, rather than
  rows of a reconciler beside the sandboxes'; the pods carry no release
  label.
- **Routes are realised for HTTP** by a route door of `ferund`'s front
  door with route tokens in a header of their own, where the proposal
  left them to an ingress outside this ADR and ADR 0015 keeps inbound
  traffic out of the front doors: previews pass the port door already,
  and there is no ingress.
- **An attached service is a supervised process** in the sandbox's main
  container, not a container added to the running pod: nothing is needed
  of the node or of ADR 0004 and ADR 0007.
- **Names**: an addon's reference resolves to its address, a service's
  to its name in a hosts block that `ferund` writes, rather than
  `feosd`'s resolver or a file the guest writes.
- **Attachable sandboxes** and the groups `env-` and `sbx-` with the
  egress table: the sandbox API gains `attachable`, which the proposal
  kept unchanged.
- **Images are pinned to digests at apply**, and releases carry
  **annotations**; a base that moved is a conflict naming the current
  release, not `FAILED_PRECONDITION`.
- **Volume data identity** and the **failed-release fallback** for
  addons, and the **release deadline**, beyond the services' budgets.
- **A job runs once at a time** in its environment, across releases,
  where the proposal let any job run whenever asked.
- **Clone takes a whole spec**, not values to change (plan sizes,
  replicas): a caller that wants other values renders its own spec and
  passes it, as FeCode does.
- **Restarts and readiness run in `ferund`** until the guest supervises
  containers (ADR 0004 set that alternative aside; it stands as the
  interim).
- **Credentials** reach pods as environment variables at start, are
  generated once and never rotated; secrets reach pods as environment
  variables only, and the log tails `ferund` keeps are redacted of both.
- **Later**: MCP tools and grants for environments, an admin's listing,
  volumes for services and their mounts, data copied on a clone, addon
  backups and minor upgrades, a CLI, routes' hosts.

## Alternatives considered

- **GitOps: the declaration as a file in the repository, a controller that
  pulls it** (Flux). What Git gives, history, rollback, a draft per branch,
  the declaration travelling with the code, the release model gives too,
  except the last, which FeCode handles at its landing. What Git lacks is
  what the operator asks for: history per environment, a copy of an
  environment, tags on states, and above all an answer at the moment of
  writing. An agent that writes a file learns of its mistake at deploy
  time, in another layer's words. The format question exists only because
  the declaration is a file. A caller who wants Git in front runs `apply`
  from its CI.
- **Compose as the declaration.** The most natural thing a model writes,
  and it says nothing of managed addons, routes, scale to zero,
  environments or secrets by environment; what we added would live in
  `x-` extensions, which is where the platform's semantics would be. As an
  import it is useful; as the API it would make the platform's objects
  second-class.
- **Mutable objects without releases**, Kubernetes's model: every change
  in place, `resourceVersion` for concurrency, history nobody keeps.
  Kubernetes leaves history to Git, and operators build release tracking
  around it. Nomad's job versions with `plan`, `history` and `revert`,
  Heroku's releases and CloudFormation's change sets are the idea here,
  and the control plane's model borrows from Nomad already (ADR 0015).
- **Drafts in `ferund`**: a stored desired state the reconciler ignores
  until it is applied. The draft is the caller's, FeCode's workspace or a
  CI's branch. Plan with a `base` gives what a stored plan is for, the
  consistency of plan and apply, without an owner, a lifetime and a sweep
  for drafts.
- **An automatic revert in `ferund`.** A release the operator did not
  make, in a history that is supposed to be theirs. The mechanism, keeping
  the last realised release serving, is `ferund`'s; the policy is the
  caller's.
- **A generic store of kinds** with a schema per kind, as custom resources
  are: the generality of Kubernetes without its ecosystem. Six kinds,
  typed and validated against each other, are what the callers need.
- **Instances in tables of their own**, with a reconciler beside the
  sandboxes' engine: a second engine for placement, bookings, ops,
  hibernation, drain and tombstones, which the sandboxes have settled.
- **A container added to the sandbox's pod for an attached service**:
  a node mechanism (ADR 0004, ADR 0007) for what a process in the main
  container, which already sees the working tree, does.
- **Names from `feosd`'s resolver** (ADR 0010): objects sent to every
  node like groups, and a resolver per pod. A hosts block needs nothing of
  the node and is written where `ferund` writes files already.

## Consequences

- `ferund` gains environments, five of the six kinds, releases with plan,
  revert, tags and annotations, clone, incidents, and a controller for
  services, jobs and addons beside the sandboxes' engine, which runs their
  pods. The sandboxes' patterns carry over: rows, ops, pinned specs,
  tombstones. The REST API grows with them; the management UI shows
  environments, their releases, incidents and logs, read only.
- A service's pods are pods like any other (ADR 0004). The node knows
  nothing of environments or releases; labels carry them, and the node
  sees only pods, volumes and security groups.
- The sandbox API gains `attachable`; an environment's pods are no
  tenant's sandboxes and are absent from its routes. A sandbox is not an
  object of an environment; an attached service names one.
- `ferund` sends security groups and creates volumes on nodes (ADR 0012,
  ADR 0016) for the first time, and carries application traffic in its
  front door until an ingress does.
- **Known exposure**: a pod's environment, secrets resolved, reaches
  `feosd`'s record of the pod, `GetPod` and `ListPods`, the node's disk
  and hibernation checkpoints in the clear, and the NATS addon's
  password is in its server's arguments inside its pod. The database
  holds no value: the log tails it keeps are redacted of the
  environment's secrets and addon credentials before they are stored, as
  far as matching the values and their escaped form in a URL reaches,
  while the logs `ferund` reads for a caller from a running pod are not.
  `feosd` sealing the pod record is the follow-up.
- This ADR relies on decisions it does not make: images from private
  registries (ADR 0001's open question; the sandboxes pull public images
  alone) and an ingress that realises routes for hosts, TLS and
  protocols beyond HTTP. A caller that wants a text declaration edits one
  through the API, validated per operation, and exports the file.
  FeCode's side, a definition per project, environments per class,
  promotion, what its agent may apply, is FeCode's ADR 0008.

## Open questions

- The ingress: a service of its own in front of the nodes or a role of
  `ferund`'s instances, hosts, ACME, the way in for IPv4 clients; until
  then the route door serves HTTP with route tokens.
- The revert route takes no annotations: it copies release K's and adds
  `ferun.revertOf`. A caller that marks its reverts, FeCode's autoRevert,
  applies K's spec through the releases route instead; whether revert
  takes annotations of the caller's.
- A service's budget counts from the release's start (moved on only by
  the time the release waited for another release's run of a job), so
  that a release job of its own that runs long eats into it while the
  release's deadline would allow more; whether a service's budget starts
  when it may move.
- Restarts and readiness in the guest, as ADR 0004 decides, and what
  `ferund` then keeps of them; attached services have no crash loop yet,
  and are routed by the port of the matched release, not their own.
- `feosd` sealing the pod record (the known exposure), and rotating an
  addon's credentials, which today are never changed.
- The logs answer is read from the pod and not redacted, while the
  tails `ferund` keeps are, and only of the values as they are and as a
  URL escapes them: whether the answer is redacted too, at the cost of
  resolving the environment's secrets for every read, and how far
  redaction should reach.
- An addon without a volume (NATS without JetStream) that is lost is
  never replaced, although no data ties it to a node: whether it is
  recreated wherever it fits, as a service's lost replica is.
- Volumes for services; data on a clone, from an addon's backup or a
  volume's checkpoint.
- Tenancy: a FeRun tenant per FeCode project, for quotas and billing;
  today FeCode keeps its projects' environments under one tenant,
  labelled by project.
