# ADR 0018: Environments and releases: FeRun's CaaS API takes typed objects, and every change is a numbered release

**Status:** Proposed · **Date:** 2026-10-18

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
  - A **service**: a pod template per ADR 0004 (containers with roles,
    probes, a restart policy, resources), its image pinned to a digest, a
    number of replicas, the ports it serves, and its idle policy: after
    how long without a request its pods are suspended, as sandboxes are
    paused. A suspended service's pod is resumed on the first request,
    which its route holds until the pod is ready (ADR 0004).
  - A **job**: a pod that runs to its end. A job `on: release` runs before
    the release's services move (a migration); any job runs when asked.
    How often a job may fail is the environment's to say.
  - An **addon**: a managed backing service by type and version,
    `postgres`, `valkey`, `nats`, `clickhouse`; a plan, which is its
    size; and the options of its type (databases and roles for Postgres,
    JetStream for NATS). `ferund` realises an addon from a blueprint per
    type: pods, volumes (ADR 0016), credentials it generates, health, a
    backup to the object store, minor upgrades. Its outputs, addresses and
    credentials, are what services reference. An addon is one instance in
    its first version; replication is a decision per type.
  - A **route**: a host, optional, a path, a service and a port. A route
    is a declaration; what realises it, an ingress for services or FeCode's
    forwarder for previews, is outside this ADR. ADR 0015 keeps inbound
    traffic out of `ferund`'s front doors.
  - A **volume**: a persistent volume (ADR 0016) that a service's pods
    mount. A volume attaches to one pod, so a service with one has one
    replica.
  - A **secret**: a name and versioned values, stored sealed under the key
    the bindings rest at. A release pins the version; the value reaches a
    pod as an environment variable or a file, and the API never returns
    it.
- Objects reference each other by name, and the references are resolved
  when a write is validated: a service's environment variable from an
  addon's output (`{addon: postgres, role: expense_svc, key: url}`) or
  from a secret, a route's service and port, a job's image. A dangling
  reference is an error of the write, not a pod that fails at start.

### A service may run inside a sandbox

- A service's **runtime** is `image`, pods of its own as above, or
  `attached`: it runs inside a named sandbox pod of the same tenant, as a
  supervised container with a command and a working directory in the
  sandbox's tree, under the same probes, restarts and logs, with the
  environment's addons reachable by name. This is how FeCode's agent runs
  the application it is writing: from source, in its own sandbox, while
  Postgres and NATS are real. Stage and production run `image`. The node's
  mechanism for a container added to a running pod that sees the
  sandbox's working tree is a follow-up for ADR 0004 and ADR 0007 (open
  questions).

### Every change is a release

- The objects of an environment are not changed in place. A write, an
  `apply` of the whole desired state or an operation on one object (put a
  service, remove a route) that `ferund` completes to the whole, produces
  a **release**: a number, monotonic per environment; the complete desired
  state; who, when, and a message; and the release it was based on.
  Releases are immutable and kept.
- **Plan** takes the same input as a write and answers with what the write
  would do: its validation findings, each with the path of the object and
  field it concerns and what would make it pass, and the diff against the
  current release, object by object. Plan writes nothing. Apply runs the
  same validation and, when it passes, records the release. The findings
  are worded for whoever writes the objects, a model included: the path,
  the problem, the remedy.
- A write names the release it was planned against, its `base`. When the
  environment has moved since, the write fails with `FAILED_PRECONDITION`
  and the caller plans again: the compare-and-swap of ADR 0012's
  `from_version`.
- **Revert** to release K is a new release with K's content. A **tag**
  names a release (`v1.4`, `demo-2026-10`) and moves only by an explicit
  call, which is recorded. The history of an environment is the list of
  its releases with their diffs: what changed in production in October is
  a query.
- A secret's new value is a new version and a release that pins it, so
  that a rotation is in the history as a rotation, never as a value.

### The reconciler works toward the latest release

- An environment's rows follow the sandboxes' pattern: a row per instance
  (a service's pods, an addon's pods and volumes), ops under the row's
  lock, pod specs that carry environment, object and release in their
  labels, sync and reconcile as the sandboxes have them.
- A release's rollout runs its `on: release` jobs first, then moves each
  service replica by replica, a new pod ready (ADR 0004's readiness)
  before an old one goes, routes following readiness. The release is
  **realised** when every object matches it; the status names the release
  realised and the one in progress.
- A rollout that does not reach readiness within its budget has
  **failed**: the service keeps the pods of the last realised release
  serving, the new ones are removed, and the environment stays on its
  failed desired release until the next one, a revert or a fix. `ferund`
  writes no release of its own. Whether a failed rollout is reverted at
  once is a policy of the caller, FeCode's per environment class, not a
  mechanism that invents history.
- What the reconciler cannot resolve ends in an **incident** on the
  environment: the object, the condition (a failed rollout, a crash loop,
  a failed job, no capacity, an unhealthy addon, a volume that cannot
  attach), and the evidence `ferund` has: the pod's last exits and
  restarts, its log tail, the events, the release diff since the last
  realised one. An incident is opened once per object and release and
  closed when the condition clears. Incidents are the hook for whoever
  operates: a person, or FeCode's agent.

### An environment is copied from a release

- `clone` makes a new environment from a release of another: the same
  objects, with the values changed that the caller names, plan sizes,
  replicas, hosts. Secrets are copied only where each is named. An addon's
  data comes along only when asked, from the addon's backup; a volume's
  from its checkpoint (ADR 0016). A copy for a new region and a demo for a
  customer are a call and a plan; idle, the copy costs what suspended pods
  cost.

### One API for every caller

- FeCode's backend for its projects' environments, an agent through
  FeRun's MCP tools (plan, apply, status, logs and incidents on the
  environments its token may see), a person through a CLI or FeCode's UI,
  a customer's CI: all use the same API. It accepts objects. A bundle, the
  whole desired state as one JSON document, is one input of `apply` and
  the export of a release. GitOps is then a way a caller chooses to drive
  the API, not the architecture.

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

## Consequences

- `ferund` gains environments, the six kinds, releases with plan, revert
  and tags, clone, incidents, and a reconciler for services and addons
  beside the sandboxes'. The sandboxes' patterns carry over: rows, ops,
  pinned specs, tombstones. The REST API, MCP and the activity log grow
  with them; the management UI shows environments and their releases.
- A service's pods are pods like any other (ADR 0004). The node knows
  nothing of environments or releases; labels carry them.
- The sandbox API is unchanged. A sandbox is not an object of an
  environment; an attached service names one.
- This ADR relies on decisions it does not make: images from private
  registries (ADR 0001's open question; the sandboxes pull public images
  alone), an ingress that realises routes, name resolution per environment
  (`feosd`'s resolver of ADR 0010, answering a pod's queries for its
  environment's names, is the candidate), the node's side of attached
  services, and a blueprint per addon type.
- A caller that wants a text declaration edits one through the API,
  validated per operation, and exports the file. FeCode's side, a
  definition per project, environments per class, promotion, what its
  agent may apply, is FeCode's ADR.

## Open questions

- How a container is added to a running sandbox pod so that it sees the
  sandbox's working tree: a container of the pod over a shared mount, or
  a supervisor in the main container.
- Name resolution in an environment: `feosd`'s resolver answering per pod
  from objects the control plane sends like security groups, or
  `/etc/hosts` written by the guest at each release.
- Addon credentials into pods: environment variables at start, with a
  restart on rotation, or a file the application re-reads; and whether a
  loopback proxy of ADR 0014 can carry a database protocol later.
- Tenancy: a FeRun tenant per FeCode project, or environments labelled by
  project under one tenant.
- The budget of a rollout and of a job.
- The ingress: a service of its own in front of the nodes or a role of
  `ferund`'s instances, ACME, the way in for IPv4 clients.
